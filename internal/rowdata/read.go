package rowdata

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// A read pages through a table in key order. The page is ordered by the key
// columns the request names and then by the table's primary key columns the
// request leaves out, so the order is total even when the requested key is not
// unique, and the next page starts strictly after the last row of this one: a
// page token carries that row's values of the ordering columns. A read outside
// a transaction reads each page as of its own moment, so a row written between
// two pages behind the cursor is not read again and one written ahead of it is
// read.

// maxToken is the longest page token the contract carries.
const maxToken = 4096

// Order is the column sequence a read orders its rows by: the requested key
// columns, then the primary key columns of the table the request leaves out.
func Order(keys, primaryKey []string) []string {
	order := slices.Clone(keys)
	for _, column := range primaryKey {
		if !slices.Contains(order, column) {
			order = append(order, column)
		}
	}
	return order
}

// PageQuery is the statement that reads one page.
type PageQuery struct {
	// Table is the quoted table path.
	Table string
	// Columns are the columns the statement selects, in order.
	Columns []string
	// Order are the ordering columns and OrderTypes their YDB types.
	Order      []string
	OrderTypes []string
	// After are the ordering column values of the last row of the previous
	// page; nil reads the first page.
	After []*protocol.DataValue
	// Limit is the most rows the statement returns.
	Limit uint64
}

// Text is the YQL statement of the page:
//
//	SELECT `a`, `b` FROM `dir/t` WHERE <after> ORDER BY `a`, `b` LIMIT n
//
// The predicate selects the rows strictly after After in the order:
// (o1 > v1) OR (o1 = v1 AND o2 > v2) OR ..., where a null equals only a null
// (o IS NULL) and every value sorts after a null (o IS NOT NULL), which is how
// YDB orders an optional column.
func (q PageQuery) Text() (string, error) {
	quoted := func(columns []string) string {
		out := make([]string, len(columns))
		for i, column := range columns {
			out[i] = ydbtype.QuoteIdentifier(column)
		}
		return strings.Join(out, ", ")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "SELECT %s FROM %s", quoted(q.Columns), q.Table)
	if q.After != nil {
		where, err := q.after()
		if err != nil {
			return "", err
		}
		b.WriteString(" WHERE " + where)
	}
	fmt.Fprintf(&b, " ORDER BY %s LIMIT %d", quoted(q.Order), q.Limit)
	return b.String(), nil
}

func (q PageQuery) after() (string, error) {
	if len(q.After) != len(q.Order) || len(q.OrderTypes) != len(q.Order) {
		return "", errors.New("the page token does not match the order of the read")
	}
	equal := make([]string, len(q.Order))
	greater := make([]string, len(q.Order))
	for i, column := range q.Order {
		name := ydbtype.QuoteIdentifier(column)
		if _, null := q.After[i].GetValue().(*protocol.DataValue_Null); null {
			equal[i], greater[i] = name+" IS NULL", name+" IS NOT NULL"
			continue
		}
		value, _, err := GoValue(q.After[i])
		if err != nil {
			return "", err
		}
		literal, err := ydbtype.ValueLiteral(q.OrderTypes[i], value)
		if err != nil {
			return "", err
		}
		equal[i], greater[i] = name+" = "+literal, name+" > "+literal
	}
	branches := make([]string, len(q.Order))
	for i := range q.Order {
		branches[i] = "(" + strings.Join(append(slices.Clone(equal[:i]), greater[i]), " AND ") + ")"
	}
	return strings.Join(branches, " OR "), nil
}

// token is what a page token carries: the ordering columns and the values the
// last row of the page holds in them.
type token struct {
	Order  []string        `json:"order"`
	Values json.RawMessage `json:"values"`
}

// EncodeToken writes the page token that resumes a read after a row whose
// ordering columns hold values. A row whose key is too long for a token is an
// error.
func EncodeToken(order []string, values []*protocol.DataValue) (string, error) {
	encoded, err := protojson.Marshal(&protocol.Row{Values: values})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(token{Order: order, Values: encoded})
	if err != nil {
		return "", err
	}
	text := base64.RawURLEncoding.EncodeToString(payload)
	if len(text) > maxToken {
		return "", errors.New("the key of the last row of the page is longer than a page token holds")
	}
	return text, nil
}

// DecodeToken reads a page token for a read ordered by order. A token another
// read wrote is refused.
func DecodeToken(text string, order []string) ([]*protocol.DataValue, error) {
	payload, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return nil, errors.New("the page token is not one this provider wrote")
	}
	var decoded token
	if err := json.Unmarshal(payload, &decoded); err != nil || !slices.Equal(decoded.Order, order) {
		return nil, errors.New("the page token is not one this provider wrote for this read")
	}
	var row protocol.Row
	if err := protojson.Unmarshal(decoded.Values, &row); err != nil || len(row.GetValues()) != len(order) {
		return nil, errors.New("the page token is not one this provider wrote for this read")
	}
	return row.GetValues(), nil
}

// ColumnType is the YDB type a table description reports for a column,
// without the Optional wrapper of a nullable one: Utf8, Decimal(22,9).
func ColumnType(yql string) string {
	name := strings.TrimSpace(yql)
	for {
		inner, wrapped := strings.CutPrefix(name, "Optional<")
		if !wrapped || !strings.HasSuffix(inner, ">") {
			break
		}
		name = strings.TrimSuffix(inner, ">")
	}
	return strings.ReplaceAll(name, " ", "")
}

// scannedDecimal is what ydb-go-sdk hands database/sql for a Decimal value:
// the 128-bit two's complement integer of the unscaled value, big-endian, with
// the column's precision and scale. The SDK's type is internal to it, so the
// value is recognized by the method.
type scannedDecimal interface {
	Decimal() (bytes [16]byte, precision uint32, scale uint32)
}

// Scanned is the neutral value of a value database/sql scanned from a column
// of ydbType. A moment comes back in UTC, the only zone YDB stores, where the
// driver hands it over in the local zone, and a decimal at its column's
// scale.
func Scanned(ydbType string, value any) (*protocol.DataValue, error) {
	if value == nil {
		return Null(), nil
	}
	switch Kind(ydbType) {
	case protocol.DataKind_DATA_KIND_INT64:
		if number, ok := integerValue(value); ok && number.IsInt64() {
			return &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: number.Int64()}}, nil
		}
	case protocol.DataKind_DATA_KIND_UINT64:
		if number, ok := integerValue(value); ok && number.IsUint64() {
			return &protocol.DataValue{Value: &protocol.DataValue_Uint64Value{Uint64Value: number.Uint64()}}, nil
		}
	case protocol.DataKind_DATA_KIND_BOOL:
		if flag, ok := value.(bool); ok {
			return &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: flag}}, nil
		}
	case protocol.DataKind_DATA_KIND_DOUBLE:
		switch number := value.(type) {
		case float32:
			return &protocol.DataValue{Value: &protocol.DataValue_DoubleValue{DoubleValue: float64(number)}}, nil
		case float64:
			return &protocol.DataValue{Value: &protocol.DataValue_DoubleValue{DoubleValue: number}}, nil
		}
	case protocol.DataKind_DATA_KIND_DECIMAL:
		if decimal, ok := value.(scannedDecimal); ok {
			bytes, precision, scale := decimal.Decimal()
			return &protocol.DataValue{Value: &protocol.DataValue_DecimalValue{DecimalValue: &protocol.DecimalValue{
				Digits: twosComplement(bytes).String(), Precision: precision, Scale: scale,
			}}}, nil
		}
	case protocol.DataKind_DATA_KIND_STRING:
		if text, ok := textOf(value); ok {
			return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: text}}, nil
		}
	case protocol.DataKind_DATA_KIND_JSON:
		if text, ok := textOf(value); ok {
			return &protocol.DataValue{Value: &protocol.DataValue_JsonValue{JsonValue: text}}, nil
		}
	case protocol.DataKind_DATA_KIND_BYTES:
		switch bytes := value.(type) {
		case []byte:
			return &protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: slices.Clone(bytes)}}, nil
		case string:
			return &protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: []byte(bytes)}}, nil
		}
	case protocol.DataKind_DATA_KIND_DATE, protocol.DataKind_DATA_KIND_TIMESTAMP:
		if instant, ok := value.(time.Time); ok {
			return moment(ydbType, instant), nil
		}
	case protocol.DataKind_DATA_KIND_INTERVAL:
		if duration, ok := value.(time.Duration); ok {
			return &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Nanoseconds: int64(duration)}}}, nil
		}
	}
	return nil, fmt.Errorf("the driver returned a Go %T for a %s column", value, ydbType)
}

// textOf is the text of a value the driver returns as a string or as bytes.
func textOf(value any) (string, bool) {
	switch text := value.(type) {
	case string:
		return text, true
	case []byte:
		return string(text), true
	case fmt.Stringer:
		return text.String(), true
	default:
		return "", false
	}
}

// integerValue reads a Go integer of any width.
func integerValue(value any) (*big.Int, bool) {
	switch typed := value.(type) {
	case int:
		return big.NewInt(int64(typed)), true
	case int8:
		return big.NewInt(int64(typed)), true
	case int16:
		return big.NewInt(int64(typed)), true
	case int32:
		return big.NewInt(int64(typed)), true
	case int64:
		return big.NewInt(typed), true
	case uint:
		return new(big.Int).SetUint64(uint64(typed)), true
	case uint8:
		return new(big.Int).SetUint64(uint64(typed)), true
	case uint16:
		return new(big.Int).SetUint64(uint64(typed)), true
	case uint32:
		return new(big.Int).SetUint64(uint64(typed)), true
	case uint64:
		return new(big.Int).SetUint64(typed), true
	default:
		return nil, false
	}
}

// twosComplement reads a big-endian 128-bit two's complement integer.
func twosComplement(bytes [16]byte) *big.Int {
	high, low := binary.BigEndian.Uint64(bytes[:8]), binary.BigEndian.Uint64(bytes[8:])
	number := new(big.Int).Lsh(new(big.Int).SetUint64(high), 64)
	number.Or(number, new(big.Int).SetUint64(low))
	if high>>63 == 1 {
		number.Sub(number, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	return number
}

// Limit is the LIMIT of a page of at most maxRows rows: one more, which tells
// a last page from a full one.
func Limit(maxRows uint32) uint64 { return uint64(maxRows) + 1 }
