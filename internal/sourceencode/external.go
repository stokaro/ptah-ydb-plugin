package sourceencode

import (
	"maps"
	"slices"
	"strings"

	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ExternalSource writes a data source's exact settings and the secrets it
// names by path or name, never a secret's value. An access requirement is a
// loss.
func ExternalSource(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	intent, err := schema.DesiredExternalSource(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	if intent.GetAccess() != nil {
		return AccessLoss(Noun(schema.ExternalSourceProperties), value.GetValue().GetOwner()), nil
	}
	fields, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	root := schema.InterpretationRoot(value.GetValue())
	spec, err := externals.SourceFromIntent(intent, root)
	if err != nil {
		return loss("external data source %s: %v", Subject(value.GetValue().GetOwner()), err), nil
	}
	fields = append(fields, field(externals.AttributeSourceType, spec.SourceType), field(externals.AttributeLocation, spec.Location), field(externals.AttributeAuthMethod, spec.AuthMethod))
	fields = appendOptions(fields, format, spec.Options)
	return nativePart(format, value, fields, externalsource.YAMLKeys()[0], externalsource.GoDirectives()[0], externalsource.Decode, sameSource(root))
}

// ExternalTable writes ordered columns and exact data source and option values.
func ExternalTable(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (Result, error) {
	intent, err := schema.DesiredExternalTable(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	if intent.GetAccess() != nil {
		return AccessLoss(Noun(schema.ExternalTableProperties), value.GetValue().GetOwner()), nil
	}
	fields, result, err := localIdentity(value)
	if err != nil || result.Loss != "" {
		return result, err
	}
	root := schema.InterpretationRoot(value.GetValue())
	spec, err := externals.TableFromIntent(intent, root)
	if err != nil {
		return loss("external table %s: %v", Subject(value.GetValue().GetOwner()), err), nil
	}
	fields = append(fields, field(externals.AttributeDataSource, spec.DataSource), field(externals.AttributeLocation, spec.Location))
	fields = append(fields, columns(format, spec.Columns))
	fields = appendOptions(fields, format, spec.Options)
	return nativePart(format, value, fields, externalsource.YAMLKeys()[1], externalsource.GoDirectives()[1], externalsource.Decode, sameTable(root))
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
		declaration := columnName(column.Name) + " " + column.Type
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

// columnName is how the columns attribute writes a column's name, as the
// built-in engine wrote it: bare when it is ASCII letters, digits and
// underscores that do not start with a digit, and quoted otherwise.
func columnName(name string) string {
	for i, r := range name {
		letter := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r == '_'
		digit := r >= '0' && r <= '9' && i > 0
		if !letter && !digit {
			return ydbtype.QuoteIdentifier(name)
		}
	}
	if name == "" {
		return ydbtype.QuoteIdentifier(name)
	}
	return name
}

// sameSource reports whether two desired data sources on a database whose
// root is root state one declaration, as the comparison reads one
// (externals.SameDataSource). A read keeps the REFERENCES YDB 26.2 lists, the
// external tables over the source, which no declaration writes, so the bytes
// of a read and of what its source decodes to differ.
func sameSource(root string) sameValue {
	return func(before, after *protocol.Envelope) (bool, error) {
		read, err := sourceDeclaration(before, root)
		if err != nil {
			return false, err
		}
		decoded, err := sourceDeclaration(after, root)
		if err != nil {
			return false, nil
		}
		return externals.SameDataSource(read, decoded, root), nil
	}
}

func sourceDeclaration(value *protocol.Envelope, root string) (ydbdecl.ExternalDataSource, error) {
	intent, err := schema.DesiredExternalSource(value)
	if err != nil {
		return ydbdecl.ExternalDataSource{}, err
	}
	return externals.SourceFromIntent(intent, root)
}

// sameTable reports whether two desired external tables on a database whose
// root is root state one declaration, as the comparison reads one
// (externals.SameTable). A read states a NOT NULL column by its type alone,
// which is not Optional, where a declaration also sets not_null, so the bytes
// of a read and of what its source decodes to differ.
func sameTable(root string) sameValue {
	return func(before, after *protocol.Envelope) (bool, error) {
		read, err := tableDeclaration(before, root)
		if err != nil {
			return false, err
		}
		decoded, err := tableDeclaration(after, root)
		if err != nil {
			return false, nil
		}
		return externals.SameTable(read, decoded, root), nil
	}
}

func tableDeclaration(value *protocol.Envelope, root string) (ydbdecl.ExternalTable, error) {
	intent, err := schema.DesiredExternalTable(value)
	if err != nil {
		return ydbdecl.ExternalTable{}, err
	}
	return externals.TableFromIntent(intent, root)
}
