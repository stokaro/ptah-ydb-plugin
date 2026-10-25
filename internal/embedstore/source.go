package embedstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// sourceKeyOf reads a source's key from its table.
func (s *Store) sourceKeyOf(ctx context.Context, source *protocol.EmbeddingSource) ([]keyColumn, error) {
	name := relationPath(source.GetTable())
	described, exists, err := s.describe(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, refuse(protocol.EmbeddingRefusal_ABSENT, "source table %s does not exist", name)
	}
	return keyOf(described, source.GetKeyFields(), name)
}

// sourceColumns is what a source read selects: the key, the input fields and
// the version, each as text.
func sourceColumns(source *protocol.EmbeddingSource, alias string) []string {
	selected := make([]string, 0, len(source.GetKeyFields())+len(source.GetInputFields())+1)
	for _, field := range source.GetKeyFields() {
		selected = append(selected, asText(alias, field))
	}
	for _, field := range source.GetInputFields() {
		selected = append(selected, asText(alias, field))
	}
	if field := strings.TrimSpace(source.GetVersionField()); field != "" {
		selected = append(selected, asText(alias, field))
	}
	return selected
}

// filterOf is the specification's own condition, parenthesized, or "".
func filterOf(source *protocol.EmbeddingSource) string {
	if filter := strings.TrimSpace(source.GetFilter()); filter != "" {
		return "(" + filter + ")"
	}
	return ""
}

// ScanSource reads one keyset page after a key, in key order, under the
// filter: the cursor is compared as a tuple with the key, cast back to the
// key's types, so a scan over a table that changes while it runs neither
// skips nor repeats a row.
func (s *Store) ScanSource(ctx context.Context, request *protocol.ScanEmbeddingSourceRequest) (*protocol.EmbeddingSourcePage, error) {
	source := request.GetSource()
	key, err := s.sourceKeyOf(ctx, source)
	if err != nil {
		return nil, err
	}
	var conditions []string
	var arguments []any
	if filter := filterOf(source); filter != "" {
		conditions = append(conditions, filter)
	}
	names := make([]string, len(key))
	for i, column := range key {
		names[i] = Quote(column.name)
	}
	if after := request.GetAfter(); after != nil {
		casts := make([]string, len(key))
		for i, column := range key {
			arguments = append(arguments, after.GetComponents()[i])
			casts[i] = column.cast(len(arguments))
		}
		conditions = append(conditions, fmt.Sprintf("(%s) > (%s)", strings.Join(names, ", "), strings.Join(casts, ", ")))
	}
	limit := int(request.GetMaxRows())
	// #nosec G201 -- YQL takes no parameter for a path or a column. The names
	// come from the specification through Quote; the filter is the
	// specification's own YQL, which is what the field is for; the cursor
	// values are parameters and the limit is an integer.
	query := fmt.Sprintf("SELECT %s FROM %s", strings.Join(sourceColumns(source, ""), ", "), Quote(relationPath(source.GetTable())))
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY " + strings.Join(names, ", ") + " LIMIT " + strconv.Itoa(limit)
	rows, err := s.readSource(ctx, source, query, arguments)
	if err != nil {
		return nil, err
	}
	page := &protocol.EmbeddingSourcePage{Rows: rows, Done: len(rows) < limit}
	if len(rows) > 0 {
		page.Cursor = rows[len(rows)-1].GetKey()
	}
	return page, nil
}

// ReadSourceRows rereads rows by key, under the filter, so a row that stopped
// qualifying reads as gone too.
func (s *Store) ReadSourceRows(ctx context.Context, request *protocol.ReadEmbeddingSourceRowsRequest) (*protocol.EmbeddingSourceRows, error) {
	source := request.GetSource()
	key, err := s.sourceKeyOf(ctx, source)
	if err != nil {
		return nil, err
	}
	names, tuples, arguments := keyTuples(key, request.GetKeys(), nil)
	conditions := []string{names + " IN (" + tuples + ")"}
	if filter := filterOf(source); filter != "" {
		conditions = append(conditions, filter)
	}
	// #nosec G201 -- as ScanSource.
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s", strings.Join(sourceColumns(source, ""), ", "),
		Quote(relationPath(source.GetTable())), strings.Join(conditions, " AND "))
	rows, err := s.readSource(ctx, source, query, arguments)
	if err != nil {
		return nil, err
	}
	return &protocol.EmbeddingSourceRows{Rows: rows}, nil
}

// readSource runs a source read and turns each result into a row. A NULL key
// is refused: it would make two rows share a cursor.
func (s *Store) readSource(ctx context.Context, source *protocol.EmbeddingSource, query string, arguments []any) ([]*protocol.EmbeddingSourceRow, error) {
	result, err := s.db.QueryContext(ctx, query, named(arguments...)...)
	if err != nil {
		return nil, fmt.Errorf("read %s: %s", relationPath(source.GetTable()), statusName(err))
	}
	defer func() { _ = result.Close() }()
	keyCount, inputCount := len(source.GetKeyFields()), len(source.GetInputFields())
	versioned := strings.TrimSpace(source.GetVersionField()) != ""
	width := keyCount + inputCount
	if versioned {
		width++
	}
	var rows []*protocol.EmbeddingSourceRow
	for result.Next() {
		values := make([]sql.NullString, width)
		destinations := make([]any, width)
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := result.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("read a row of %s: %s", relationPath(source.GetTable()), statusName(err))
		}
		row := &protocol.EmbeddingSourceRow{Key: &protocol.EmbeddingKey{}}
		for i := range keyCount {
			if !values[i].Valid {
				return nil, fmt.Errorf("key column %s is NULL in %s, and a keyset scan cannot resume after a row it cannot name",
					source.GetKeyFields()[i], relationPath(source.GetTable()))
			}
			row.Key.Components = append(row.Key.Components, values[i].String)
		}
		for i := range inputCount {
			row.Fields = append(row.Fields, textValue(values[keyCount+i]))
		}
		if versioned {
			row.Version = values[width-1].String
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %s", relationPath(source.GetTable()), statusName(err))
	}
	return rows, nil
}

// textValue is an input field on the wire: null or text.
func textValue(value sql.NullString) *protocol.DataValue {
	if !value.Valid {
		return &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}
	}
	return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: value.String}}
}

// ResolveRelations answers each relation as YDB names it. A YDB path has no
// search path to resolve through: a directory relative to the root and a name
// are the relation.
func ResolveRelations(relations []*protocol.Reference) []*protocol.Reference {
	resolved := make([]*protocol.Reference, 0, len(relations))
	for _, relation := range relations {
		resolved = append(resolved, &protocol.Reference{Engine: relation.GetEngine(), Database: relation.GetDatabase(),
			Catalog: relation.GetCatalog(), Kind: relation.GetKind(), Schema: directory(relation.GetSchema()),
			Name: strings.TrimSpace(relation.GetName())})
	}
	return resolved
}

// Measure answers what a plan reports about the database. The vector type is
// YDB's own, so it is always there; the vector index is the line's
// vector_indexes. The index a specification declares is buildable when YDB
// takes its settings and the line has the capability.
func (s *Store) Measure(ctx context.Context, spec *protocol.EmbeddingSpec) (*protocol.EmbeddingMeasurement, error) {
	measured := &protocol.EmbeddingMeasurement{Capabilities: map[string]bool{"vector_type": true, "vector_index": s.caps.VectorIndexes}}
	if err := checkTarget(spec); err != nil {
		measured.Refusals = append(measured.Refusals, oneLine(err.Error()))
	}
	source, sourceExists, err := s.describe(ctx, sourcePath(spec))
	if err != nil {
		return nil, err
	}
	measured.SourceExists = sourceExists
	target, targetExists, err := s.describe(ctx, targetPath(spec))
	if err != nil {
		return nil, err
	}
	// A table of the generation's own does not exist before preparation
	// creates it, and that is not a plan that cannot run.
	measured.TargetTableExists = targetExists || ownsTable(spec)
	if targetExists {
		_, measured.TargetColumnExists = target.Column(spec.GetTarget().GetColumn())
	}
	if sourceExists {
		if _, err := keyOf(source, spec.GetSource().GetKeyFields(), sourcePath(spec)); err != nil {
			measured.Refusals = append(measured.Refusals, oneLine(err.Error()))
		}
		query := "SELECT COUNT(*) FROM " + Quote(sourcePath(spec))
		if filter := filterOf(spec.GetSource()); filter != "" {
			// #nosec G202 -- the filter is the specification's own YQL.
			query += " WHERE " + filter
		}
		var count uint64
		if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %s", sourcePath(spec), statusName(err))
		}
		measured.RowsCounted, measured.Rows = true, int64(min(count, 1<<63-1)) // #nosec G115 -- clamped into range on this line
	}
	measured.IndexBuildable = true
	if strings.TrimSpace(spec.GetTarget().GetIndexMethod()) != "" {
		_, settingsErr := settingsOf(spec)
		measured.IndexBuildable = settingsErr == nil && s.caps.VectorIndexes
	}
	return measured, nil
}
