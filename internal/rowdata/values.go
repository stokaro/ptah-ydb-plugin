// Package rowdata types, reads and writes the rows of YDB tables for the row
// services of the provider contract: TypeRows, ReadRows and RenderRowWrites.
//
// A row travels as a list of provider-neutral values, never SQL text. YQL
// types every literal and converts few of them, so each value is compared and
// written in its column's YDB type, as the Ptah host's embedded YDB engine
// does through its internal/ydbdata and migration/datadiff: the type a live
// table holds where the column exists, and the type a declaration lands on
// for the target's capabilities otherwise. internal/ydbtype holds the type
// map and the literals; this package adds the neutral values around them.
//
// # Values
//
// The canonical value of a column takes one case of the neutral value for
// each YDB type:
//
//   - a signed integer (Serial included) is int64, an unsigned one uint64;
//   - Bool is bool, Float and Double are double;
//   - Decimal(p,s) is a decimal of precision p at scale s, so 1.5 and a
//     declared 1.50 are one value;
//   - Utf8 and Uuid are text, and so is DyNumber, whose digits are its value
//     as the host keeps them;
//   - Json and JsonDocument are JSON text, JsonDocument with sorted keys and
//     no spaces, the form YDB returns it in;
//   - String and Yson are bytes;
//   - Date and Date32 are a date, the other date and time types a timestamp in
//     UTC, and Interval and Interval64 an interval of nanoseconds.
package rowdata

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// Kind is the case the canonical values of a column of ydbType take, and
// UNSPECIFIED for a type that has no value this package writes.
func Kind(ydbType string) protocol.DataKind {
	columnType := ydbtype.StorageType(ydbType)
	switch {
	case ydbtype.IsSigned(columnType):
		return protocol.DataKind_DATA_KIND_INT64
	case ydbtype.IsUnsigned(columnType):
		return protocol.DataKind_DATA_KIND_UINT64
	}
	if _, _, ok := ydbtype.DecimalArguments(columnType); ok {
		return protocol.DataKind_DATA_KIND_DECIMAL
	}
	switch columnType {
	case ydbtype.Bool:
		return protocol.DataKind_DATA_KIND_BOOL
	case ydbtype.Float, ydbtype.Double:
		return protocol.DataKind_DATA_KIND_DOUBLE
	case ydbtype.Utf8, ydbtype.UUID, ydbtype.DyNumber:
		return protocol.DataKind_DATA_KIND_STRING
	case ydbtype.JSON, ydbtype.JSONDocument:
		return protocol.DataKind_DATA_KIND_JSON
	case ydbtype.String, ydbtype.Yson:
		return protocol.DataKind_DATA_KIND_BYTES
	case ydbtype.Date, ydbtype.Date32:
		return protocol.DataKind_DATA_KIND_DATE
	case ydbtype.Datetime, ydbtype.Datetime64, ydbtype.Timestamp, ydbtype.Timestamp64:
		return protocol.DataKind_DATA_KIND_TIMESTAMP
	case ydbtype.Interval, ydbtype.Interval64:
		return protocol.DataKind_DATA_KIND_INTERVAL
	default:
		return protocol.DataKind_DATA_KIND_UNSPECIFIED
	}
}

// errNative refuses a value of a provider-defined type this provider does not
// define.
var errNative = errors.New("a native value is not a value YDB stores")

// GoValue is value as the Go value the type map reads: nil for null, and the
// Go type of the case for the others. A decimal is its digits with the point
// placed by its scale, a date is midnight UTC of its day, and an interval is a
// time.Duration, which holds no month. carried is false for a value with no
// case set, a column the row does not carry.
func GoValue(value *protocol.DataValue) (result any, carried bool, err error) {
	switch v := value.GetValue().(type) {
	case nil:
		return nil, false, nil
	case *protocol.DataValue_Null:
		return nil, true, nil
	case *protocol.DataValue_BoolValue:
		return v.BoolValue, true, nil
	case *protocol.DataValue_Int64Value:
		return v.Int64Value, true, nil
	case *protocol.DataValue_Uint64Value:
		return v.Uint64Value, true, nil
	case *protocol.DataValue_DoubleValue:
		return v.DoubleValue, true, nil
	case *protocol.DataValue_StringValue:
		return v.StringValue, true, nil
	case *protocol.DataValue_BytesValue:
		return v.BytesValue, true, nil
	case *protocol.DataValue_JsonValue:
		return v.JsonValue, true, nil
	case *protocol.DataValue_DecimalValue:
		return DecimalString(v.DecimalValue.GetDigits(), v.DecimalValue.GetScale()), true, nil
	case *protocol.DataValue_TimestampValue:
		return time.Unix(v.TimestampValue.GetSeconds(), int64(v.TimestampValue.GetNanos())).UTC(), true, nil
	case *protocol.DataValue_DateValue:
		if v.DateValue > math.MaxInt64/86400 || v.DateValue < math.MinInt64/86400 {
			return nil, true, fmt.Errorf("the date %d days from 1970-01-01 is out of range", v.DateValue)
		}
		return time.Unix(v.DateValue*86400, 0).UTC(), true, nil
	case *protocol.DataValue_IntervalValue:
		return intervalDuration(v.IntervalValue)
	case *protocol.DataValue_NativeValue:
		return nil, true, errNative
	default:
		return nil, true, fmt.Errorf("unknown value case %T", v)
	}
}

// intervalDuration reads an interval as a time.Duration. YDB's intervals are a
// count of microseconds, so a month, which has no fixed length, is refused.
func intervalDuration(interval *protocol.IntervalValue) (any, bool, error) {
	if interval.GetMonths() != 0 {
		return nil, true, errors.New("a YDB interval holds no month, which has no fixed length")
	}
	const day = int64(24 * time.Hour)
	days := interval.GetDays()
	if days > math.MaxInt64/day || days < math.MinInt64/day {
		return nil, true, errors.New("the interval is out of range")
	}
	total := days*day + interval.GetNanoseconds()
	if (interval.GetNanoseconds() > 0 && total < days*day) || (interval.GetNanoseconds() < 0 && total > days*day) {
		return nil, true, errors.New("the interval is out of range")
	}
	return time.Duration(total), true, nil
}

// DecimalString places the point of an unscaled decimal: digits 1250 at scale
// 2 is 12.50, and -5 at scale 3 is -0.005.
func DecimalString(digits string, scale uint32) string {
	negative := strings.HasPrefix(digits, "-")
	unsigned := strings.TrimPrefix(digits, "-")
	if scale > 0 {
		if pad := int(scale) + 1 - len(unsigned); pad > 0 {
			unsigned = strings.Repeat("0", pad) + unsigned
		}
		unsigned = unsigned[:len(unsigned)-int(scale)] + "." + unsigned[len(unsigned)-int(scale):]
	}
	if negative {
		return "-" + unsigned
	}
	return unsigned
}

// Canonical returns value, a Go value [ydbtype.CanonicalValue] gives for a
// column of ydbType, as the neutral value of that column's kind.
func Canonical(ydbType string, value any) (*protocol.DataValue, error) {
	if value == nil {
		return Null(), nil
	}
	switch typed := value.(type) {
	case int64:
		return &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: typed}}, nil
	case uint64:
		return &protocol.DataValue{Value: &protocol.DataValue_Uint64Value{Uint64Value: typed}}, nil
	case bool:
		return &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: typed}}, nil
	case float64:
		return &protocol.DataValue{Value: &protocol.DataValue_DoubleValue{DoubleValue: typed}}, nil
	case []byte:
		return &protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: typed}}, nil
	case time.Duration:
		return &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Nanoseconds: int64(typed)}}}, nil
	case time.Time:
		return moment(ydbType, typed), nil
	case string:
		return text(ydbType, typed)
	default:
		return nil, fmt.Errorf("no neutral value for a Go %T", value)
	}
}

// Null is the null value.
func Null() *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}
}

// moment is a date for a Date or Date32 column, whose moments are midnight
// UTC, and a timestamp for the others.
func moment(ydbType string, value time.Time) *protocol.DataValue {
	utc := value.UTC()
	if Kind(ydbType) == protocol.DataKind_DATA_KIND_DATE {
		days := utc.Unix() / 86400
		if utc.Unix() < 0 && utc.Unix()%86400 != 0 {
			days--
		}
		return &protocol.DataValue{Value: &protocol.DataValue_DateValue{DateValue: days}}
	}
	return &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{
		Seconds: utc.Unix(), Nanos: int32(utc.Nanosecond()),
	}}}
}

// text is the value of a column whose canonical value is text: a decimal at
// its column's scale for a Decimal, JSON for Json and JsonDocument, and text
// for the rest.
func text(ydbType, value string) (*protocol.DataValue, error) {
	switch Kind(ydbType) {
	case protocol.DataKind_DATA_KIND_DECIMAL:
		precision, scale, _ := ydbtype.DecimalArguments(ydbType)
		digits, err := scaledDigits(value, scale)
		if err != nil {
			return nil, err
		}
		return &protocol.DataValue{Value: &protocol.DataValue_DecimalValue{DecimalValue: &protocol.DecimalValue{
			Digits: digits, Precision: uint32(precision), Scale: uint32(scale),
		}}}, nil
	case protocol.DataKind_DATA_KIND_JSON:
		return &protocol.DataValue{Value: &protocol.DataValue_JsonValue{JsonValue: value}}, nil
	default:
		return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: value}}, nil
	}
}

// scaledDigits writes a decimal text, an optional minus and digits with an
// optional fraction of at most scale digits, as its unscaled integer at scale:
// 12.5 at scale 2 is 1250.
func scaledDigits(value string, scale int) (string, error) {
	negative := strings.HasPrefix(value, "-")
	whole, fraction, _ := strings.Cut(strings.TrimPrefix(value, "-"), ".")
	if len(fraction) > scale || strings.Trim(whole+fraction, "0123456789") != "" || whole == "" {
		return "", fmt.Errorf("the decimal %q does not fit scale %d", value, scale)
	}
	number, ok := new(big.Int).SetString(whole+fraction+strings.Repeat("0", scale-len(fraction)), 10)
	if !ok {
		return "", fmt.Errorf("the decimal %q is not digits", value)
	}
	if negative {
		number.Neg(number)
	}
	return number.String(), nil
}
