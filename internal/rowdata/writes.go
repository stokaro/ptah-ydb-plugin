package rowdata

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// RenderRowWrites renders each write as one YQL statement, in input order,
// with every value written in its column's YDB type, the type TypeRows gave
// it. The statements are the ones the host's embedded engine writes for a row
// diff (migration/datadiff): the columns of an INSERT sorted by name, the SET
// of an UPDATE holding the columns outside the key sorted by name, and a key
// predicate that names each key column in sorted order and addresses a null
// key with IS NULL. An UPSERT is YQL's own statement, UPSERT INTO, in the
// shape of the INSERT; it writes the row over the row with the same primary
// key.
//
// A value its column cannot hold, a column the write names no type for and a
// value of a provider-defined type refuse the whole batch with
// UNSUPPORTED_VALUE, naming the write and the column but never the value. The
// table is the reference's directory and name, relative to the database of the
// session that runs the statement.
func RenderRowWrites(request *protocol.RenderRowWritesRequest) (*protocol.RenderResult, error) {
	units := make([]*protocol.Unit, 0, len(request.GetWrites()))
	for _, write := range request.GetWrites() {
		if err := validateTable(write.GetTable()); err != nil {
			return nil, err
		}
		statement, err := renderWrite(write)
		if refusal, ok := errors.AsType[*writeRefusal](err); ok {
			return &protocol.RenderResult{Refusal: &protocol.RenderRefusal{
				ChangeId: write.GetChangeId(), Reason: protocol.RenderRefusal_UNSUPPORTED_VALUE, Message: OneLine(refusal.message),
			}}, nil
		}
		if err != nil {
			return nil, err
		}
		units = append(units, &protocol.Unit{ChangeId: write.GetChangeId(), Body: &protocol.Unit_Sql{Sql: statement}})
	}
	return &protocol.RenderResult{Complete: true, Units: units}, nil
}

// validateTable refuses a table reference this provider cannot name in a
// statement: one in a catalog, one under a nested path, or one of another
// engine. The directory is a path relative to the database.
func validateTable(table *protocol.Reference) error {
	if table.GetCatalog() != "" || len(table.GetPath()) != 0 || len(table.GetOverload()) != 0 ||
		(table.GetEngine() != "" && table.GetEngine() != "ydb") {
		return errors.New("a YDB row write names a table by its directory and name")
	}
	return ValidateDirectory(table.GetSchema())
}

// ValidateDirectory refuses a directory that is not a relative path below the
// database: an absolute path, a part that climbs out with `..`, or an empty
// part.
func ValidateDirectory(directory string) error {
	if directory == "" {
		return nil
	}
	for part := range strings.SplitSeq(strings.TrimRight(directory, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("the directory %q is not a path relative to the database", directory)
		}
	}
	return nil
}

// writeRefusal is a write the batch stops at, with a message that names no
// value.
type writeRefusal struct{ message string }

func (r *writeRefusal) Error() string { return r.message }

func renderWrite(write *protocol.RowWrite) (string, error) {
	values := make(map[string]*protocol.DataValue, len(write.GetColumns()))
	for i, column := range write.GetColumns() {
		values[column] = write.GetValues()[i]
	}
	table := ydbtype.QuotePath(write.GetTable().GetSchema(), write.GetTable().GetName())
	literal := func(column, role string) (string, error) {
		return columnLiteral(write.GetColumnTypes()[column], column, role, values[column])
	}
	switch write.GetMode() {
	case protocol.RowWrite_INSERT, protocol.RowWrite_UPSERT:
		columns := slices.Sorted(slices.Values(write.GetColumns()))
		names := make([]string, len(columns))
		literals := make([]string, len(columns))
		for i, column := range columns {
			value, err := literal(column, "column")
			if err != nil {
				return "", err
			}
			names[i], literals[i] = ydbtype.QuoteIdentifier(column), value
		}
		verb := "INSERT"
		if write.GetMode() == protocol.RowWrite_UPSERT {
			verb = "UPSERT"
		}
		return fmt.Sprintf("%s INTO %s (%s) VALUES (%s);", verb, table, strings.Join(names, ", "), strings.Join(literals, ", ")), nil
	case protocol.RowWrite_UPDATE_BY_KEY:
		var assignments []string
		for _, column := range slices.Sorted(slices.Values(write.GetColumns())) {
			if slices.Contains(write.GetKeyColumns(), column) {
				continue
			}
			value, err := literal(column, "column")
			if err != nil {
				return "", err
			}
			assignments = append(assignments, ydbtype.QuoteIdentifier(column)+" = "+value)
		}
		where, err := keyPredicate(write.GetKeyColumns(), values, literal)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("UPDATE %s SET %s WHERE %s;", table, strings.Join(assignments, ", "), where), nil
	case protocol.RowWrite_DELETE_BY_KEY:
		where, err := keyPredicate(write.GetKeyColumns(), values, literal)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("DELETE FROM %s WHERE %s;", table, where), nil
	default:
		return "", fmt.Errorf("unknown row write mode %s", write.GetMode())
	}
}

// keyPredicate is `key = literal` for each key column in sorted order, joined
// with AND. A key column holding null is addressed with IS NULL: written as
// `= NULL` the predicate holds for no row, and the statement changes nothing.
func keyPredicate(keys []string, values map[string]*protocol.DataValue, literal func(column, role string) (string, error)) (string, error) {
	parts := make([]string, 0, len(keys))
	for _, key := range slices.Sorted(slices.Values(keys)) {
		if _, null := values[key].GetValue().(*protocol.DataValue_Null); null {
			parts = append(parts, ydbtype.QuoteIdentifier(key)+" IS NULL")
			continue
		}
		value, err := literal(key, "key column")
		if err != nil {
			return "", err
		}
		parts = append(parts, ydbtype.QuoteIdentifier(key)+" = "+value)
	}
	return strings.Join(parts, " AND "), nil
}

// columnLiteral writes value for a column of ydbType. A column with no type,
// a value of a provider-defined type and a value the column cannot hold are a
// *writeRefusal naming the column and why, never the value.
func columnLiteral(ydbType, column, role string, value *protocol.DataValue) (string, error) {
	if strings.TrimSpace(ydbType) == "" {
		return "", &writeRefusal{message: fmt.Sprintf("%s %q: YDB stores a literal only in its column's own type, "+
			"and the write names no type for this column", role, column)}
	}
	goValue, _, err := GoValue(value)
	if err != nil {
		return "", &writeRefusal{message: fmt.Sprintf("%s %q: %s cannot hold the value: %s", role, column, ydbType, err)}
	}
	literal, err := ydbtype.ValueLiteral(ydbType, goValue)
	if refused, ok := errors.AsType[*ydbtype.ValueError](err); ok {
		return "", &writeRefusal{message: fmt.Sprintf("%s %q: %s cannot hold the value: %s", role, column, refused.Type, refused.Reason)}
	}
	if err != nil {
		return "", &writeRefusal{message: fmt.Sprintf("%s %q: %s", role, column, err)}
	}
	return literal, nil
}

// maxLine is the most bytes a provider-authored line a host may show holds.
const maxLine = 512

// OneLine is text cut to the longest prefix of whole characters that fits
// one diagnostic line. The text it is given names columns with %q, so it holds
// no line break.
func OneLine(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= maxLine {
		return text
	}
	cut := maxLine
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimSpace(text[:cut])
}

func cloneValue(value *protocol.DataValue) *protocol.DataValue {
	return proto.CloneOf(value)
}
