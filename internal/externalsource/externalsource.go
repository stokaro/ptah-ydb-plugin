// Package externalsource decodes the external data sources and external
// tables a YAML document or a Go file declares, with the syntax and refusals
// of the host's bundled YDB source (internal/ydbsource): the YAML document
// keys external_data_sources and external_tables, and the Go directives
// ptah:schema:externaldatasource and ptah:schema:externaltable.
package externalsource

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// The syntax this package claims.
const (
	// KeyDataSources is the YAML document key that declares data sources.
	KeyDataSources = "external_data_sources"
	// KeyTables is the YAML document key that declares external tables.
	KeyTables = "external_tables"
	// DirectiveDataSource declares one data source in Go.
	DirectiveDataSource = "ptah:schema:externaldatasource"
	// DirectiveTable declares one external table in Go.
	DirectiveTable = "ptah:schema:externaltable"
)

// The fields of one column of an external table a YAML document declares.
const (
	columnName    = "name"
	columnType    = "type"
	columnNotNull = "not_null"
)

func sourceAttributes() []string {
	return []string{externals.AttributeName, externals.AttributeSchema, externals.AttributeSourceType, externals.AttributeLocation, externals.AttributeAuthMethod, externals.AttributeOptions}
}

func tableAttributes() []string {
	return []string{externals.AttributeName, externals.AttributeSchema, externals.AttributeDataSource, externals.AttributeLocation, externals.AttributeColumns, externals.AttributeOptions}
}

// YAMLKeys declares the YAML document keys external_data_sources and
// external_tables.
func YAMLKeys() []*protocol.YAMLSourceKey {
	return []*protocol.YAMLSourceKey{
		{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: KeyDataSources, Selector: KeyDataSources, Models: []string{schema.ExternalSourceProperties}},
		{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: KeyTables, Selector: KeyTables, Models: []string{schema.ExternalTableProperties}},
	}
}

// GoDirectives declares the Go directives of a data source and of an
// external table, with the attributes of the host's own directives of the
// same names.
func GoDirectives() []*protocol.GoSourceDirective {
	return []*protocol.GoSourceDirective{
		{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectiveDataSource, Selector: DirectiveDataSource, Attributes: sourceAttributes(), Models: []string{schema.ExternalSourceProperties}},
		{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectiveTable, Selector: DirectiveTable, Attributes: tableAttributes(), Models: []string{schema.ExternalTableProperties}},
	}
}

// Owns reports whether a fragment carries one of this package's claims.
func Owns(fragment *protocol.SourceFragment) bool {
	if key := fragment.GetYamlKey(); key != nil {
		return key.GetScope() == protocol.YAMLSourceKey_DOCUMENT && (key.GetKey() == KeyDataSources || key.GetKey() == KeyTables)
	}
	directive := fragment.GetGoDirective().GetDirective()
	return directive == DirectiveDataSource || directive == DirectiveTable
}

// declared is one external object a document declares, with the fragment and
// path its trace points at.
type declared struct {
	fragment int
	path     []*protocol.SourcePathStep
	kind     string
	schema   string
	name     string
	source   ydbdecl.ExternalDataSource
	table    ydbdecl.ExternalTable
}

func (d declared) noun() string {
	if d.kind == schema.ExternalSourceProperties {
		return "external data source"
	}
	return "external table"
}

// Decode reads every external object a document declares and returns the
// values of each fragment it owns, by index. An invalid declaration refuses
// the whole document.
func Decode(ctx context.Context, document *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error) {
	var objects []declared
	owned := map[int]bool{}
	for index, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) {
			continue
		}
		owned[index] = true
		if fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
			return nil, fmt.Errorf("unsupported YDB external object source attachment")
		}
		mapping := fragment.GetValue().GetMapping()
		var read []declared
		var err error
		switch {
		case document.Format == protocol.SourceDocument_YAML && (fragment.Selector == KeyDataSources || fragment.Selector == KeyTables):
			if mapping == nil && fragment.GetValue().GetNull() == nil {
				return nil, fmt.Errorf("%s requires a mapping", fragment.Selector)
			}
			read, err = yamlObjects(index, fragment.Selector, mapping)
		case document.Format == protocol.SourceDocument_GO_ANNOTATIONS && (fragment.Selector == DirectiveDataSource || fragment.Selector == DirectiveTable):
			var object declared
			object, err = goObject(fragment.Selector, mapping)
			object.fragment = index
			read = []declared{object}
		default:
			return nil, fmt.Errorf("unsupported YDB external object source format")
		}
		if err != nil {
			return nil, err
		}
		objects = append(objects, read...)
	}
	result := make(map[int][]*protocol.SourceModelValue, len(owned))
	for index := range owned {
		result[index] = nil
	}
	seen := map[string]bool{}
	for position, object := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		display := ydbdecl.ExternalDisplay(object.schema, object.name)
		if key := object.kind + "\x00" + display; seen[key] {
			return nil, fmt.Errorf("%s %s is declared twice", object.noun(), display)
		}
		seen[object.kind+"\x00"+display] = true
		value, err := capture(object)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", object.noun(), display, err)
		}
		fragment := document.Fragments[object.fragment]
		result[object.fragment] = append(result[object.fragment], &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
			Value: &protocol.NormalizedValue{Id: "external." + strconv.Itoa(position), Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
				CanonicalFormat: schema.NativeDesiredModel(object.kind).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)},
			Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT, Path: object.path}})
	}
	return result, nil
}

func capture(object declared) (*protocol.Envelope, error) {
	owner := &protocol.Reference{Engine: "ydb", Kind: object.kind, Schema: object.schema, Name: object.name}
	if err := schema.ValidateNativeOwner(owner); err != nil {
		return nil, err
	}
	if object.kind == schema.ExternalSourceProperties {
		intent, err := externals.SourceIntent(object.source)
		if err != nil {
			return nil, err
		}
		return schema.CaptureExternalIntent(owner, intent)
	}
	intent, err := externals.TableIntent(object.table)
	if err != nil {
		return nil, err
	}
	return schema.CaptureExternalIntent(owner, intent)
}

// path reads a declaration's directory and name, as the host keeps them.
func path(object *declared, directory, name string) error {
	ref, err := ydbdecl.ExternalPath(directory, name)
	if err != nil {
		return err
	}
	object.schema, object.name = ref.Schema, ref.Name
	return nil
}

// goObject reads one directive: a data source or an external table.
func goObject(directive string, mapping *protocol.SourceMapping) (declared, error) {
	values, err := attributes(directive, mapping)
	if err != nil {
		return declared{}, err
	}
	var object declared
	if directive == DirectiveDataSource {
		object.kind = schema.ExternalSourceProperties
		options, err := externals.ParseOptions(values[externals.AttributeOptions], externals.DataSourceReserved...)
		if err != nil {
			return declared{}, err
		}
		if err := path(&object, values[externals.AttributeSchema], values[externals.AttributeName]); err != nil {
			return declared{}, err
		}
		object.source = ydbdecl.ExternalDataSource{SourceType: strings.TrimSpace(values[externals.AttributeSourceType]), Location: strings.TrimSpace(values[externals.AttributeLocation]),
			AuthMethod: strings.TrimSpace(values[externals.AttributeAuthMethod]), Options: options}
		return object, ydbdecl.ValidateDataSource(object.source)
	}
	object.kind = schema.ExternalTableProperties
	columns, err := externals.ParseColumns(values[externals.AttributeColumns])
	if err != nil {
		return declared{}, err
	}
	options, err := externals.ParseOptions(values[externals.AttributeOptions], externals.TableReserved...)
	if err != nil {
		return declared{}, err
	}
	if err := path(&object, values[externals.AttributeSchema], values[externals.AttributeName]); err != nil {
		return declared{}, err
	}
	object.table = ydbdecl.ExternalTable{DataSource: strings.TrimSpace(values[externals.AttributeDataSource]), Location: strings.TrimSpace(values[externals.AttributeLocation]),
		Columns: columns, Options: options}
	return object, ydbdecl.ValidateExternalTable(object.table)
}

// yamlObjects reads the mapping of one YAML key: each entry an object keyed
// by its name, which a name field overrides, in key order.
func yamlObjects(fragment int, key string, mapping *protocol.SourceMapping) ([]declared, error) {
	entries := slices.Clone(mapping.GetEntries())
	slices.SortFunc(entries, func(a, b *protocol.SourceEntry) int { return strings.Compare(a.Key, b.Key) })
	var result []declared
	for _, entry := range entries {
		read := yamlSource
		noun := "external data source"
		if key == KeyTables {
			read, noun = yamlTable, "external table"
		}
		object, err := read(entry)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", noun, entry.Key, err)
		}
		object.fragment = fragment
		object.path = []*protocol.SourcePathStep{{Step: &protocol.SourcePathStep_Key{Key: entry.Key}}}
		result = append(result, object)
	}
	return result, nil
}

func yamlSource(entry *protocol.SourceEntry) (declared, error) {
	fields, options, err := yamlFields(entry, []string{externals.AttributeName, externals.AttributeSchema, externals.AttributeSourceType, externals.AttributeLocation, externals.AttributeAuthMethod}, false)
	if err != nil {
		return declared{}, err
	}
	checked, err := ydbdecl.CheckExternalOptions(options, externals.DataSourceReserved...)
	if err != nil {
		return declared{}, err
	}
	object := declared{kind: schema.ExternalSourceProperties}
	if err := yamlPath(&object, fields, entry.Key); err != nil {
		return declared{}, err
	}
	object.source = ydbdecl.ExternalDataSource{SourceType: strings.TrimSpace(fields[externals.AttributeSourceType]), Location: strings.TrimSpace(fields[externals.AttributeLocation]),
		AuthMethod: strings.TrimSpace(fields[externals.AttributeAuthMethod]), Options: checked}
	return object, ydbdecl.ValidateDataSource(object.source)
}

func yamlTable(entry *protocol.SourceEntry) (declared, error) {
	fields, options, err := yamlFields(entry, []string{externals.AttributeName, externals.AttributeSchema, externals.AttributeDataSource, externals.AttributeLocation}, true)
	if err != nil {
		return declared{}, err
	}
	checked, err := ydbdecl.CheckExternalOptions(options, externals.TableReserved...)
	if err != nil {
		return declared{}, err
	}
	object := declared{kind: schema.ExternalTableProperties}
	if err := yamlPath(&object, fields, entry.Key); err != nil {
		return declared{}, err
	}
	columns, err := yamlColumns(entry)
	if err != nil {
		return declared{}, err
	}
	if err := ydbdecl.CheckExternalColumns(columns); err != nil {
		return declared{}, err
	}
	object.table = ydbdecl.ExternalTable{DataSource: strings.TrimSpace(fields[externals.AttributeDataSource]), Location: strings.TrimSpace(fields[externals.AttributeLocation]),
		Columns: columns, Options: checked}
	return object, ydbdecl.ValidateExternalTable(object.table)
}

// yamlPath reads an entry's directory and name, the entry's key unless a
// name field names it, which holds no slash.
func yamlPath(object *declared, fields map[string]string, key string) error {
	name := fields[externals.AttributeName]
	if name == "" {
		name = key
	}
	if strings.Contains(name, "/") {
		return &ydbdecl.ExternalError{Attribute: externals.AttributeName, Reason: fmt.Sprintf("%q holds a slash; name the directory with %s", name, externals.AttributeSchema)}
	}
	return path(object, fields[externals.AttributeSchema], name)
}

// yamlFields reads an entry's scalar fields of names and its options
// mapping, and refuses a field of another name. An entry that takes columns
// holds them in a field yamlColumns reads.
func yamlFields(entry *protocol.SourceEntry, names []string, columns bool) (map[string]string, map[string]string, error) {
	mapping := entry.GetValue().GetMapping()
	if mapping == nil {
		return nil, nil, fmt.Errorf("an entry requires a mapping")
	}
	fields := map[string]string{}
	var options map[string]string
	for _, field := range mapping.GetEntries() {
		switch {
		case field.Key == externals.AttributeOptions:
			if field.GetValue().GetNull() != nil {
				continue
			}
			values := field.GetValue().GetMapping()
			if values == nil {
				return nil, nil, fmt.Errorf("%s requires a mapping", field.Key)
			}
			options = map[string]string{}
			for _, option := range values.GetEntries() {
				text, err := scalar(option.GetValue())
				if err != nil {
					return nil, nil, fmt.Errorf("%s %s: %w", field.Key, option.Key, err)
				}
				options[option.Key] = text
			}
		case field.Key == externals.AttributeColumns && columns:
		case slices.Contains(names, field.Key):
			text, err := scalar(field.GetValue())
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", field.Key, err)
			}
			fields[field.Key] = text
		default:
			return nil, nil, fmt.Errorf("unknown key %q", field.Key)
		}
	}
	return fields, options, nil
}

// yamlColumns reads the columns sequence of an external table's entry: each
// item a mapping of a name, a type and not_null.
func yamlColumns(entry *protocol.SourceEntry) ([]ydbdecl.ExternalColumn, error) {
	var columns []ydbdecl.ExternalColumn
	for _, field := range entry.GetValue().GetMapping().GetEntries() {
		if field.Key != externals.AttributeColumns || field.GetValue().GetNull() != nil {
			continue
		}
		sequence := field.GetValue().GetSequence()
		if sequence == nil {
			return nil, fmt.Errorf("%s requires a sequence", field.Key)
		}
		for position, item := range sequence.GetValues() {
			mapping := item.GetMapping()
			if mapping == nil {
				return nil, fmt.Errorf("%s[%d] requires a mapping", field.Key, position)
			}
			var column ydbdecl.ExternalColumn
			for _, part := range mapping.GetEntries() {
				switch part.Key {
				case columnName, columnType:
					text, err := scalar(part.GetValue())
					if err != nil {
						return nil, fmt.Errorf("%s[%d] %s: %w", field.Key, position, part.Key, err)
					}
					if part.Key == columnName {
						column.Name = strings.TrimSpace(text)
					} else {
						column.Type = strings.TrimSpace(text)
					}
				case columnNotNull:
					switch value := part.GetValue().GetValue().(type) {
					case *protocol.SourceValue_Boolean:
						column.NotNull = value.Boolean
					case *protocol.SourceValue_Null:
					default:
						return nil, fmt.Errorf("%s[%d] %s requires true or false", field.Key, position, part.Key)
					}
				default:
					return nil, fmt.Errorf("%s[%d]: unknown key %q", field.Key, position, part.Key)
				}
			}
			columns = append(columns, column)
		}
	}
	return columns, nil
}

// attributes reads a Go directive's attributes, which the host lexed as text.
func attributes(directive string, mapping *protocol.SourceMapping) (map[string]string, error) {
	if mapping == nil {
		return nil, fmt.Errorf("%s requires attributes", directive)
	}
	values := map[string]string{}
	for _, entry := range mapping.GetEntries() {
		text, ok := entry.GetValue().GetValue().(*protocol.SourceValue_Text)
		if !ok {
			return nil, fmt.Errorf("attribute %s requires text", entry.Key)
		}
		values[entry.Key] = text.Text
	}
	return values, nil
}

// scalar reads a YAML scalar as the text the host's bundled source reads it
// as: a number keeps its digits, a boolean reads true or false, and a null
// is a value written empty.
func scalar(value *protocol.SourceValue) (string, error) {
	switch content := value.GetValue().(type) {
	case *protocol.SourceValue_Text:
		return content.Text, nil
	case *protocol.SourceValue_Number:
		return content.Number, nil
	case *protocol.SourceValue_Boolean:
		return strconv.FormatBool(content.Boolean), nil
	case *protocol.SourceValue_Null:
		return "", nil
	default:
		return "", fmt.Errorf("expected a scalar")
	}
}
