package embedstore

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// keyColumn is one column of a source key, with the type a key value read as
// text is cast back to.
type keyColumn struct {
	name     string
	ydbType  string
	nullable bool
}

// cast renders a parameter cast to the column's type. A key travels as text,
// the one rendering every key type has; YQL compares typed values only, so the
// text is cast back before it meets the column, which keeps the comparison on
// the primary key a read is ordered by.
func (k keyColumn) cast(parameter int) string {
	return fmt.Sprintf("CAST($p%d AS %s)", parameter, k.ydbType)
}

// keyOf matches key fields against a described table's primary key. On YDB a
// generation reads and writes a table by its primary key: it orders the
// table, so a page is a range read, and it identifies a row, so a write by it
// touches one row. Key fields that are anything else are refused.
func keyOf(described RowTable, fields []string, name string) ([]keyColumn, error) {
	if !slices.Equal(described.PrimaryKey, fields) {
		return nil, refuse(protocol.EmbeddingRefusal_UNSUPPORTED,
			"key_fields %s are not the primary key of %s, which is %s. On YDB a generation reads and writes a table by its "+
				"primary key, so key_fields name it, in key order", quotedList(fields), name, quotedList(described.PrimaryKey))
	}
	key := make([]keyColumn, 0, len(fields))
	for _, field := range fields {
		column, found := described.Column(field)
		if !found {
			return nil, refuse(protocol.EmbeddingRefusal_ABSENT, "table %s has no column %s", name, field)
		}
		key = append(key, keyColumn{name: field, ydbType: column.Type, nullable: column.Nullable})
	}
	return key, nil
}

// quotedList renders names for a diagnostic.
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = strconv.Quote(name)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// keyMatch renders the predicate that addresses one key: each column equal to
// its parameter cast back to the column's type, numbered from first.
func keyMatch(alias string, key []keyColumn, first int) string {
	conditions := make([]string, len(key))
	for i, column := range key {
		conditions[i] = qualified(alias, column.name) + " = " + column.cast(first+i)
	}
	return strings.Join(conditions, " AND ")
}

// keyTuples renders the IN list of keys, with their components appended to
// arguments, and the key columns it compares.
func keyTuples(key []keyColumn, keys []*protocol.EmbeddingKey, arguments []any) (names, tuples string, bound []any) {
	quoted := make([]string, len(key))
	for i, column := range key {
		quoted[i] = Quote(column.name)
	}
	rendered := make([]string, 0, len(keys))
	for _, value := range keys {
		casts := make([]string, len(key))
		for i, column := range key {
			arguments = append(arguments, value.GetComponents()[i])
			casts[i] = column.cast(len(arguments))
		}
		rendered = append(rendered, "("+strings.Join(casts, ", ")+")")
	}
	return "(" + strings.Join(quoted, ", ") + ")", strings.Join(rendered, ", "), arguments
}

// qualified renders a column under an alias, or bare when there is none.
func qualified(alias, column string) string {
	if alias == "" {
		return Quote(column)
	}
	return alias + "." + Quote(column)
}

// asText renders a column read as text, which is how a key, an input field
// and a version travel.
func asText(alias, column string) string {
	return "CAST(" + qualified(alias, column) + " AS Utf8)"
}

// keyID renders a key for a map lookup. The NUL separator cannot occur in a
// component, which the SDK refuses.
func keyID(components []string) string {
	return strings.Join(components, "\x00")
}
