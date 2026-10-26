package sourceencode

import (
	"maps"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// ExternalSource writes a data source's exact settings and secret references.
// Access constraints without a source spelling remain an explicit loss.
func ExternalSource(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	intent, err := schema.DesiredExternalSource(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	fields, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	spec, err := externals.SourceFromIntent(intent, "")
	if err != nil {
		return Result{Loss: "The source grammar requires a complete external data source declaration"}, nil
	}
	fields = append(fields, field(externals.AttributeSourceType, spec.SourceType), field(externals.AttributeLocation, spec.Location), field(externals.AttributeAuthMethod, spec.AuthMethod))
	fields = appendOptions(fields, format, spec.Options)
	return nativePart(format, value, fields, externalsource.YAMLKeys()[0], externalsource.GoDirectives()[0], externalsource.Decode)
}

// ExternalTable writes ordered columns and exact data source and option values.
func ExternalTable(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	intent, err := schema.DesiredExternalTable(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	fields, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	spec, err := externals.TableFromIntent(intent, "")
	if err != nil {
		return Result{Loss: "The source grammar requires a complete external table declaration"}, nil
	}
	fields = append(fields, field(externals.AttributeDataSource, spec.DataSource), field(externals.AttributeLocation, spec.Location))
	fields = append(fields, columns(format, spec.Columns))
	fields = appendOptions(fields, format, spec.Options)
	return nativePart(format, value, fields, externalsource.YAMLKeys()[1], externalsource.GoDirectives()[1], externalsource.Decode)
}

func appendOptions(fields []*protocol.SourceEntry, format protocol.SourceDocument_Format, options map[string]string) []*protocol.SourceEntry {
	if len(options) == 0 {
		return fields
	}
	var entries []*protocol.SourceEntry
	var pairs []string
	escape := strings.NewReplacer(`\`, `\\`, `;`, `\;`)
	for _, key := range slices.Sorted(maps.Keys(options)) {
		entries = append(entries, field(key, options[key]))
		pairs = append(pairs, key+"="+escape.Replace(options[key]))
	}
	value := mapping(entries...)
	if format == protocol.SourceDocument_GO_ANNOTATIONS {
		value = text(strings.Join(pairs, ";"))
	}
	return append(fields, &protocol.SourceEntry{Key: externals.AttributeOptions, Value: value})
}

func columns(format protocol.SourceDocument_Format, columns []ydbdecl.ExternalColumn) *protocol.SourceEntry {
	var values []*protocol.SourceValue
	var declarations []string
	for _, column := range columns {
		values = append(values, mapping(field("name", column.Name), field("type", column.Type), &protocol.SourceEntry{
			Key: "not_null", Value: &protocol.SourceValue{Value: &protocol.SourceValue_Boolean{Boolean: column.NotNull}},
		}))
		declaration := ydbtype.QuoteIdentifier(column.Name) + " " + column.Type
		if column.NotNull {
			declaration += " NOT NULL"
		}
		declarations = append(declarations, declaration)
	}
	value := &protocol.SourceValue{Value: &protocol.SourceValue_Sequence{Sequence: &protocol.SourceSequence{Values: values}}}
	if format == protocol.SourceDocument_GO_ANNOTATIONS {
		value = text(strings.Join(declarations, ", "))
	}
	return &protocol.SourceEntry{Key: externals.AttributeColumns, Value: value}
}
