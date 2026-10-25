package rowdata

import (
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// TypeRows answers, for a YDB table, the YDB type of each column and the
// declared and live rows with every typed value in its canonical form, as
// the host's embedded engine does in internal/ydbdata.
//
// YQL types every literal, so each value is written in its column's YDB type,
// and that type is the live table's where the column exists: YDB cannot change
// a column's type, so what the table holds is what a statement writes into. A
// column the table does not have yet takes the type its declaration lands on
// for the request's capabilities, which is the type the schema stage of the
// same plan creates it with. A column neither side types keeps its values.
//
// The rows are compared in canonical form: declared as `12.50` or as a moment
// in another zone, a value pairs with the 12.5 a Decimal reads back and with
// the moment in UTC a read returns, so a converged row plans nothing. A value
// its column cannot hold is an error naming the row, counted from 1, the
// column and the type. Rows keep their order and the columns each carries.
func TypeRows(request *protocol.TypeRowsRequest) (*protocol.TypedRows, error) {
	caps := ydbtype.Capabilities(request.GetCapabilities())
	columnTypes := make(map[string]string, len(request.GetColumns()))
	for _, column := range request.GetLiveTable().GetColumns() {
		columnTypes[column.GetName()] = column.GetType()
	}
	for _, column := range request.GetColumns() {
		if _, live := columnTypes[column.GetName()]; live || column.GetDeclaredType() == "" {
			continue
		}
		mapping, err := ydbtype.Map(column.GetDeclaredType(), caps)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", column.GetName(), err)
		}
		columnTypes[column.GetName()] = mapping.Type
	}
	result := &protocol.TypedRows{Columns: make([]*protocol.DataColumn, len(request.GetColumns()))}
	names := make([]string, len(request.GetColumns()))
	for i, column := range request.GetColumns() {
		names[i] = column.GetName()
		ydbType := columnTypes[column.GetName()]
		kind := protocol.DataKind_DATA_KIND_UNSPECIFIED
		if ydbType != "" {
			kind = Kind(ydbType)
			if kind == protocol.DataKind_DATA_KIND_UNSPECIFIED {
				return nil, fmt.Errorf("column %q: %w", column.GetName(),
					&ydbtype.Refusal{Declared: "a value of type " + ydbType, Reason: "Ptah writes no literal of that type"})
			}
		}
		result.Columns[i] = &protocol.DataColumn{Name: column.GetName(), Type: ydbType, Kind: kind}
	}
	var err error
	if result.Declared, err = canonicalRows(names, columnTypes, request.GetDeclared()); err != nil {
		return nil, fmt.Errorf("declared %w", err)
	}
	if result.Live, err = canonicalRows(names, columnTypes, request.GetLive()); err != nil {
		return nil, fmt.Errorf("live %w", err)
	}
	return result, nil
}

// canonicalRows returns copies of rows with every value of a typed column in
// its canonical form. A value with no case set stays unset.
func canonicalRows(names []string, columnTypes map[string]string, rows []*protocol.Row) ([]*protocol.Row, error) {
	if rows == nil {
		return nil, nil
	}
	out := make([]*protocol.Row, len(rows))
	for i, row := range rows {
		typed := &protocol.Row{Values: make([]*protocol.DataValue, len(row.GetValues()))}
		for j, value := range row.GetValues() {
			canonical, err := canonicalValue(columnTypes[names[j]], value)
			if err != nil {
				return nil, fmt.Errorf("row %d, column %q: %w", i+1, names[j], err)
			}
			typed.Values[j] = canonical
		}
		out[i] = typed
	}
	return out, nil
}

// canonicalValue is value in the canonical form of a column of ydbType, and
// value itself for an untyped column or a value the row does not carry.
func canonicalValue(ydbType string, value *protocol.DataValue) (*protocol.DataValue, error) {
	goValue, carried, err := GoValue(value)
	switch {
	case !carried:
		return &protocol.DataValue{}, nil
	case ydbType == "":
		return cloneValue(value), nil
	case err != nil:
		return nil, &ydbtype.ValueError{Type: ydbType, Value: "the value", Reason: err.Error()}
	}
	canonical, err := ydbtype.CanonicalValue(ydbType, goValue)
	if err != nil {
		return nil, err
	}
	return Canonical(ydbType, canonical)
}
