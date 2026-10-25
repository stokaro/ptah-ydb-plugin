package externals

import (
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// The attributes of the Go directives and the fields of the YAML entries that
// declare external objects, the host's names.
const (
	AttributeName       = "name"
	AttributeSchema     = "schema"
	AttributeSourceType = "source_type"
	AttributeLocation   = "location"
	AttributeAuthMethod = "auth_method"
	AttributeOptions    = "options"
	AttributeDataSource = "data_source"
	AttributeColumns    = "columns"
)

// ParseOptions reads an options attribute, `NAME=value` pairs separated by
// `;`, into a map keyed by upper-case name. Each value is kept as written,
// without the spaces around it; `\;` writes a semicolon into a value, as
// `CSV_DELIMITER=\;` does, and `\\` a backslash. reserved are the names the
// declaration writes through attributes of their own, which the attribute may
// not repeat.
func ParseOptions(text string, reserved ...string) (map[string]string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	var names, values []string
	for _, pair := range splitEscaped(text) {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		name, value, found := strings.Cut(pair, "=")
		if !found || strings.TrimSpace(name) == "" {
			return nil, &ydbdecl.ExternalError{Attribute: AttributeOptions, Reason: fmt.Sprintf("%q is not NAME=value", strings.TrimSpace(pair))}
		}
		names = append(names, name)
		values = append(values, value)
	}
	// Each pair is checked in the order it is written, as a map could not
	// keep two pairs of one name apart.
	checked := make(map[string]string, len(names))
	for i, name := range names {
		one, err := ydbdecl.CheckExternalOptions(map[string]string{name: values[i]}, reserved...)
		if err != nil {
			return nil, err
		}
		for upper, value := range one {
			if _, repeated := checked[upper]; repeated {
				return nil, &ydbdecl.ExternalError{Attribute: AttributeOptions, Reason: upper + " is written twice"}
			}
			checked[upper] = value
		}
	}
	if len(checked) == 0 {
		return nil, nil
	}
	return checked, nil
}

// splitEscaped splits text at every `;` a backslash does not escape, and
// reads `\;` as a semicolon and `\\` as a backslash.
func splitEscaped(text string) []string {
	var (
		parts   []string
		current strings.Builder
	)
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '\\' && i+1 < len(text) && (text[i+1] == ';' || text[i+1] == '\\'):
			i++
			current.WriteByte(text[i])
		case text[i] == ';':
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(text[i])
		}
	}
	return append(parts, current.String())
}

// ParseColumns reads a columns attribute: `name Type [NOT NULL]` entries
// separated by commas outside parentheses and angle brackets, so `amount
// Decimal(22,9)` is one column. A name may be quoted with backticks. DEFAULT,
// PRIMARY KEY and FAMILY are refused: the server drops a default without a
// word, and refuses the other two.
func ParseColumns(text string) ([]ydbdecl.ExternalColumn, error) {
	var columns []ydbdecl.ExternalColumn
	for _, entry := range splitTopLevel(text) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, &ydbdecl.ExternalError{Attribute: AttributeColumns, Reason: "an entry is empty"}
		}
		column, err := parseColumn(entry)
		if err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	if err := ydbdecl.CheckExternalColumns(columns); err != nil {
		return nil, err
	}
	return columns, nil
}

// splitTopLevel splits text at the commas outside parentheses and angle
// brackets.
func splitTopLevel(text string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range text {
		switch r {
		case '(', '<':
			depth++
		case ')', '>':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, text[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, text[start:])
}

// parseColumn reads one `name Type [NOT NULL]` entry.
func parseColumn(entry string) (ydbdecl.ExternalColumn, error) {
	name, rest, err := columnName(entry)
	if err != nil {
		return ydbdecl.ExternalColumn{}, err
	}
	words := strings.Fields(rest)
	upper := strings.ToUpper(strings.Join(words, " "))
	for _, refused := range []string{"DEFAULT", "PRIMARY KEY", "FAMILY"} {
		if strings.Contains(" "+upper+" ", " "+refused+" ") {
			return ydbdecl.ExternalColumn{}, &ydbdecl.ExternalError{Attribute: AttributeColumns,
				Reason: fmt.Sprintf("column %s carries %s, which an external table does not take", name, refused)}
		}
	}
	column := ydbdecl.ExternalColumn{Name: name}
	switch {
	case strings.HasSuffix(" "+upper, " NOT NULL"):
		column.NotNull = true
		words = words[:len(words)-2]
	case strings.HasSuffix(" "+upper, " NULL"):
		words = words[:len(words)-1]
	}
	column.Type = strings.Join(words, " ")
	return column, nil
}

// columnName reads the name at the start of entry, bare or backticked, and
// returns the rest.
func columnName(entry string) (name, rest string, err error) {
	if inner, ok := strings.CutPrefix(entry, "`"); ok {
		end := strings.IndexByte(inner, '`')
		if end <= 0 {
			return "", "", &ydbdecl.ExternalError{Attribute: AttributeColumns, Reason: fmt.Sprintf("%q opens a quoted name it does not close", entry)}
		}
		return inner[:end], inner[end+1:], nil
	}
	name, rest, _ = strings.Cut(entry, " ")
	return name, rest, nil
}
