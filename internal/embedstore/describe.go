package embedstore

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// RowTable is what a generation reads of one row table: its columns in
// declared order, its key, its indexes and its user attributes, which is
// where YDB keeps a comment Ptah wrote.
type RowTable struct {
	Columns    []RowColumn
	PrimaryKey []string
	Indexes    []RowIndex
	Attributes map[string]string
}

// RowColumn is one column: its type as YQL spells it, without Optional.
type RowColumn struct {
	Name     string
	Type     string
	Nullable bool
}

// spelling is the column's type for a diagnostic.
func (c RowColumn) spelling() string {
	if c.Nullable {
		return "Optional<" + c.Type + ">"
	}
	return c.Type
}

// RowIndex is one index. Vector is set for a vector index, and Ready reports
// an index that finished building, which YDB serves queries through.
type RowIndex struct {
	Name    string
	Columns []string
	Vector  *Vector
	Ready   bool
}

// Vector is what a vector_kmeans_tree index orders by: its metric as its YQL
// setting, its element type and its dimension.
type Vector struct {
	Metric    string
	Type      string
	Dimension uint32
}

// Column finds a column by name.
func (t RowTable) Column(name string) (RowColumn, bool) {
	position := slices.IndexFunc(t.Columns, func(c RowColumn) bool { return c.Name == name })
	if position < 0 {
		return RowColumn{}, false
	}
	return t.Columns[position], true
}

// Index finds an index by name.
func (t RowTable) Index(name string) (RowIndex, bool) {
	position := slices.IndexFunc(t.Indexes, func(i RowIndex) bool { return i.Name == name })
	if position < 0 {
		return RowIndex{}, false
	}
	return t.Indexes[position], true
}

// exists reports whether a row table holds the path, relative to the root.
// Another kind of object there is an error.
func (s *Store) exists(ctx context.Context, relative string) (bool, error) {
	scheme := Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(s.driver))
	response, err := scheme.DescribePath(ctx, &Ydb_Scheme.DescribePathRequest{Path: s.absolute(relative)})
	if err != nil {
		return false, fmt.Errorf("describe %s: transport failed", relative)
	}
	if response.GetOperation().GetStatus() == Ydb.StatusIds_SCHEME_ERROR {
		return false, nil
	}
	var entry Ydb_Scheme.DescribePathResult
	if err := nativeapi.Operation(response.GetOperation(), &entry); err != nil {
		return false, fmt.Errorf("describe %s: %w", relative, err)
	}
	if kind := entry.GetSelf().GetType(); kind != Ydb_Scheme.Entry_TABLE {
		return false, fmt.Errorf("YDB object %s is a %s, not a row table", relative, strings.ToLower(kind.String()))
	}
	return true, nil
}

// describe reads the row table at path, relative to the root. exists is false
// when nothing holds the path; another kind of object there is an error.
func (s *Store) describe(ctx context.Context, relative string) (RowTable, bool, error) {
	exists, err := s.exists(ctx, relative)
	if err != nil || !exists {
		return RowTable{}, false, err
	}
	absolute := s.absolute(relative)
	session, err := nativeapi.OpenTable(ctx, s.driver)
	if err != nil {
		return RowTable{}, false, err
	}
	defer func() { _ = session.Close() }()
	described, err := session.Describe(ctx, absolute)
	if err != nil {
		return RowTable{}, false, fmt.Errorf("describe %s: %w", relative, err)
	}
	table, err := rowTable(described)
	if err != nil {
		return RowTable{}, false, fmt.Errorf("describe %s: %w", relative, err)
	}
	return table, true, nil
}

func rowTable(described *native.DescribeTableResult) (RowTable, error) {
	table := RowTable{PrimaryKey: slices.Clone(described.GetPrimaryKey()), Attributes: described.GetAttributes()}
	for _, meta := range described.GetColumns() {
		name, nullable, err := schema.ColumnType(meta.GetType())
		if err != nil {
			return RowTable{}, fmt.Errorf("column %s: %w", meta.GetName(), err)
		}
		table.Columns = append(table.Columns, RowColumn{Name: meta.GetName(), Type: name, Nullable: nullable})
	}
	for _, index := range described.GetIndexes() {
		read := RowIndex{Name: index.GetName(), Columns: slices.Clone(index.GetIndexColumns()),
			Ready: index.GetStatus() == native.TableIndexDescription_STATUS_READY}
		if vector := index.GetGlobalVectorKmeansTreeIndex(); vector != nil {
			settings := vector.GetVectorSettings().GetSettings()
			read.Vector = &Vector{Metric: metricSetting(settings.GetMetric()), Type: vectorType(settings.GetVectorType()),
				Dimension: settings.GetVectorDimension()}
		}
		table.Indexes = append(table.Indexes, read)
	}
	return table, nil
}

// metricSetting is a vector index metric as its YQL setting.
func metricSetting(metric native.VectorIndexSettings_Metric) string {
	switch metric {
	case native.VectorIndexSettings_SIMILARITY_INNER_PRODUCT:
		return "similarity=inner_product"
	case native.VectorIndexSettings_SIMILARITY_COSINE:
		return "similarity=cosine"
	case native.VectorIndexSettings_DISTANCE_COSINE:
		return "distance=cosine"
	case native.VectorIndexSettings_DISTANCE_MANHATTAN:
		return "distance=manhattan"
	case native.VectorIndexSettings_DISTANCE_EUCLIDEAN:
		return "distance=euclidean"
	default:
		return ""
	}
}

// vectorType is a vector index element type as its YQL setting.
func vectorType(kind native.VectorIndexSettings_VectorType) string {
	return strings.ToLower(strings.TrimPrefix(kind.String(), "VECTOR_TYPE_"))
}

// setAttribute writes one user attribute of the table at relative, or removes
// it when value is empty, through the table service.
func (s *Store) setAttribute(ctx context.Context, relative, key, value string) error {
	session, err := nativeapi.OpenTable(ctx, s.driver)
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	return session.Alter(ctx, &Ydb_Table.AlterTableRequest{Path: s.absolute(relative), AlterAttributes: map[string]string{key: value}})
}

// absolute resolves a path relative to the root.
func (s *Store) absolute(relative string) string {
	return path.Join(s.root, relative)
}
