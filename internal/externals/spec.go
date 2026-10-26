// Package externals reads YDB's external data sources and external tables as
// the declarations Ptah's embedded YDB engine keeps for them, compares them
// as that engine compares them, and writes the statements that create,
// replace and drop them.
//
// Both representations of the provider's models, the native description
// inspection reads and the desired intent a declaration decodes into, are read
// into the host's specifications (ydbdecl.ExternalDataSource and
// ydbdecl.ExternalTable), and the comparison, the refusals and the statements
// work on those, ported from the host's dialect/ydb/ydbexternal with its rules
// and words. Neither object holds data in YDB: a read of an external table, or
// a query naming a data source, fetches rows from another system.
package externals

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
)

// The options a statement writes from attributes of their own, and the one
// the server adds to a description.
const (
	optionSourceType = "SOURCE_TYPE"
	optionLocation   = "LOCATION"
	optionAuthMethod = "AUTH_METHOD"
	optionDataSource = "DATA_SOURCE"
	// optionReferences lists the external tables over a data source. The
	// server maintains it, and it is not a declaration's to write.
	optionReferences = "REFERENCES"
	// optionPartitionedBy is the one table option whose value is a JSON
	// array of column names rather than a string.
	optionPartitionedBy = "PARTITIONED_BY"
	// secretPathSuffix ends the name of every option that names a YDB secret
	// by its path.
	secretPathSuffix = "_SECRET_PATH"
	// objectStorage is the one source type an external table reads.
	objectStorage = "ObjectStorage"
)

// Reserved option names: those a declaration of each object writes with an
// attribute of its own.
var (
	DataSourceReserved = []string{optionSourceType, optionLocation, optionAuthMethod}
	TableReserved      = []string{optionDataSource, optionLocation}
)

// SourceFromObserved reads a data source's description as the declaration
// that describes it, as the host's reader does: the auth method out of the
// properties, REFERENCES left out, and each secret path relative to root, the
// database the read describes.
func SourceFromObserved(observed *Ydb_Table.DescribeExternalDataSourceResult, root string) (ydbdecl.ExternalDataSource, error) {
	return sourceSpec(observed.GetSourceType(), observed.GetLocation(), observed.GetProperties(), root)
}

// SourceFromIntent reads a desired data source as the declaration it states.
// An intent converted from a read holds REFERENCES, which is left out.
func SourceFromIntent(intent *native.ExternalSourceIntent, root string) (ydbdecl.ExternalDataSource, error) {
	if intent == nil {
		return ydbdecl.ExternalDataSource{}, fmt.Errorf("missing YDB external data source declaration")
	}
	return sourceSpec(intent.GetSourceType(), intent.GetLocation(), intent.GetProperties(), root)
}

func sourceSpec(sourceType, location string, properties map[string]string, root string) (ydbdecl.ExternalDataSource, error) {
	spec := ydbdecl.ExternalDataSource{SourceType: sourceType, Location: location}
	options := make(map[string]string, len(properties))
	for name, value := range properties {
		name = strings.ToUpper(name)
		switch {
		case name == optionAuthMethod:
			spec.AuthMethod = value
			continue
		case name == optionReferences:
			continue
		case strings.HasSuffix(name, secretPathSuffix):
			value = RelativePath(value, root)
		}
		options[name] = value
	}
	if len(options) > 0 {
		spec.Options = options
	}
	if err := ydbdecl.ValidateDataSource(spec); err != nil {
		return ydbdecl.ExternalDataSource{}, err
	}
	return spec, nil
}

// SourceIntent writes a declared data source as the provider's desired
// model: the auth method and every option in the properties, as the server
// keeps them.
func SourceIntent(spec ydbdecl.ExternalDataSource) (*native.ExternalSourceIntent, error) {
	if err := ydbdecl.ValidateDataSource(spec); err != nil {
		return nil, err
	}
	properties := maps.Clone(spec.Options)
	if properties == nil {
		properties = map[string]string{}
	}
	properties[optionAuthMethod] = spec.AuthMethod
	return &native.ExternalSourceIntent{SourceType: new(spec.SourceType), Location: new(spec.Location), Properties: properties}, nil
}

// TableFromObserved reads an external table's description as the
// declaration that describes it, as the host's reader does: the data source
// relative to root, the columns in order with their types and nullability,
// and each option's value out of the JSON array of one string the server
// writes, PARTITIONED_BY kept as the array.
func TableFromObserved(observed *native.DescribeExternalTableResult, root string) (ydbdecl.ExternalTable, error) {
	return tableSpec(observed.GetDataSourcePath(), observed.GetLocation(), observed.GetContent(), observed.GetColumns(), root)
}

// TableFromIntent reads a desired external table as the declaration it
// states.
func TableFromIntent(intent *native.ExternalTableIntent, root string) (ydbdecl.ExternalTable, error) {
	if intent == nil {
		return ydbdecl.ExternalTable{}, fmt.Errorf("missing YDB external table declaration")
	}
	columns := make([]*native.ColumnMeta, 0, len(intent.GetColumns()))
	for _, column := range intent.GetColumns() {
		columns = append(columns, &native.ColumnMeta{Name: column.GetName(), Type: column.GetType(), NotNull: column.NotNull})
	}
	return tableSpec(intent.GetDataSourcePath(), intent.GetLocation(), intent.GetContent(), columns, root)
}

func tableSpec(dataSource, location string, content map[string]string, columns []*native.ColumnMeta, root string) (ydbdecl.ExternalTable, error) {
	spec := ydbdecl.ExternalTable{DataSource: RelativePath(dataSource, root), Location: location}
	for _, column := range columns {
		name, nullable, err := schema.ColumnType(column.GetType())
		if err != nil {
			return ydbdecl.ExternalTable{}, fmt.Errorf("column %s: %w", column.GetName(), err)
		}
		if column.GetNotNull() {
			nullable = false
		}
		spec.Columns = append(spec.Columns, ydbdecl.ExternalColumn{Name: column.GetName(), Type: name, NotNull: !nullable})
	}
	options := make(map[string]string, len(content))
	for name, value := range content {
		option, err := DescribedTableOption(name, value)
		if err != nil {
			return ydbdecl.ExternalTable{}, err
		}
		options[strings.ToUpper(name)] = option
	}
	if len(options) > 0 {
		spec.Options = options
	}
	if err := ydbdecl.ValidateExternalTable(spec); err != nil {
		return ydbdecl.ExternalTable{}, err
	}
	return spec, nil
}

// TableIntent writes a declared external table as the provider's desired
// model: its columns as YDB types, a nullable one as Optional, and each
// option as the JSON array of one string the server describes it with. The
// source type is ObjectStorage, the one source YDB reads an external table
// from (`Only ObjectStorage source type supported`).
func TableIntent(spec ydbdecl.ExternalTable) (*native.ExternalTableIntent, error) {
	if err := ydbdecl.ValidateExternalTable(spec); err != nil {
		return nil, err
	}
	intent := &native.ExternalTableIntent{SourceType: new(objectStorage), DataSourcePath: new(spec.DataSource), Location: new(spec.Location), Content: map[string]string{}}
	for _, column := range spec.Columns {
		storage, err := schema.StorageType(column.Type, !column.NotNull)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", column.Name, err)
		}
		declared := &native.ColumnDeclaration{Name: column.Name, Type: storage, Settings: &native.ColumnIntent{}}
		if column.NotNull {
			declared.NotNull = new(true)
		}
		intent.Columns = append(intent.Columns, declared)
	}
	for _, name := range slices.Sorted(maps.Keys(spec.Options)) {
		value := spec.Options[name]
		if name == optionPartitionedBy {
			intent.Content[name] = compactList(value)
			continue
		}
		encoded, err := json.Marshal([]string{value})
		if err != nil {
			return nil, err
		}
		intent.Content[name] = string(encoded)
	}
	return intent, nil
}

// RelativePath is a path an option or a description holds, relative to root,
// the database a read describes: an absolute path under root loses root, and
// any other path is kept as it is.
func RelativePath(value, root string) string {
	if strings.Trim(root, "/") == "" {
		return value
	}
	root = "/" + strings.Trim(root, "/")
	if rest, under := strings.CutPrefix(value, root+"/"); under && strings.HasPrefix(value, "/") {
		return path.Clean(rest)
	}
	return value
}

// ResolveSource reads the path of a data source an external table names, as
// a directory and a name relative to root: relative as written, or absolute
// under root. It reports false for a path outside root or one that names no
// object.
func ResolveSource(root, written string) (ydbdecl.Path, bool) {
	trimmed := strings.TrimSpace(written)
	if strings.HasPrefix(trimmed, "/") {
		if strings.Trim(root, "/") == "" {
			return ydbdecl.Path{}, false
		}
		relative, under := strings.CutPrefix(path.Clean(trimmed), "/"+strings.Trim(root, "/")+"/")
		if !under {
			return ydbdecl.Path{}, false
		}
		trimmed = relative
	}
	ref, err := ydbdecl.ParseExternalPath(trimmed)
	return ref, err == nil
}

// DescribedTableOption is the value a declaration writes for name, given the
// value DescribeExternalTable returned for it: the one string of its JSON
// array, or for PARTITIONED_BY the array itself.
func DescribedTableOption(name, described string) (string, error) {
	if strings.ToUpper(name) == optionPartitionedBy {
		return compactList(described), nil
	}
	var list []string
	if err := json.Unmarshal([]byte(described), &list); err != nil || len(list) != 1 {
		return "", fmt.Errorf("external table option %s is %s, where the server writes a JSON array of one string", name, described)
	}
	return list[0], nil
}

// compactList writes value, a JSON array of strings, without spaces, or
// returns it unchanged when it is not one.
func compactList(value string) string {
	var list []string
	if err := json.Unmarshal([]byte(value), &list); err != nil {
		return value
	}
	compact, err := json.Marshal(list)
	if err != nil {
		return value
	}
	return string(compact)
}
