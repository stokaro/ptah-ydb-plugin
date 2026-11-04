package statementrows_test

import (
	"math"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/statementrows"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func argumentType(scalar protocol.ArgumentType_Scalar, optional, list bool) *protocol.ArgumentType {
	return &protocol.ArgumentType{Scalar: scalar, Optional: optional, List: list}
}

func signed(v int64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: v}}
}

func unsigned(v uint64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Uint64Value{Uint64Value: v}}
}

func double(v float64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_DoubleValue{DoubleValue: v}}
}

func text(v string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: v}}
}

var null = &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}

// Each typed argument binds at the YDB type of its scalar, an optional type as
// Optional<T> with a typed null, and a list as List<T>, empty or not.
func TestTypedArgument_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		argument *protocol.StatementArgument
		kind     string
		yql      string
	}{
		{name: "untyped null", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UNTYPED, false, false), Value: null},
			kind: "Void", yql: "Void()"},
		{name: "bool", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_BOOL, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: true}}}, kind: "Bool", yql: "true"},
		{name: "int8", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT8, false, false), Value: signed(math.MinInt8)},
			kind: "Int8", yql: "-128t"},
		{name: "int16", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT16, false, false), Value: signed(math.MaxInt16)},
			kind: "Int16", yql: "32767s"},
		{name: "int32", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT32, false, false), Value: signed(math.MinInt32)},
			kind: "Int32", yql: "-2147483648"},
		{name: "int64", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT64, false, false), Value: signed(-5)},
			kind: "Int64", yql: "-5l"},
		{name: "uint8", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT8, false, false), Value: unsigned(math.MaxUint8)},
			kind: "Uint8", yql: "255ut"},
		{name: "uint16", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT16, false, false), Value: unsigned(math.MaxUint16)},
			kind: "Uint16", yql: "65535us"},
		{name: "uint32", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT32, false, false), Value: unsigned(math.MaxUint32)},
			kind: "Uint32", yql: "4294967295u"},
		{name: "uint64", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT64, false, false), Value: unsigned(math.MaxUint64)},
			kind: "Uint64", yql: "18446744073709551615ul"},
		{name: "float", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_FLOAT32, false, false), Value: double(1.5)},
			kind: "Float", yql: "Float(\"1.5\")"},
		{name: "float NaN", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_FLOAT32, false, false), Value: double(math.NaN())},
			kind: "Float", yql: "Float(\"NaN\")"},
		{name: "double", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_FLOAT64, false, false), Value: double(0.1)},
			kind: "Double", yql: "Double(\"0.1\")"},
		{name: "utf8", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_STRING, false, false), Value: text("CZ")},
			kind: "Utf8", yql: "\"CZ\"u"},
		{name: "bytes", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_BYTES, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: []byte("CZ")}}}, kind: "String", yql: "\"CZ\""},
		{name: "timestamp", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_TIMESTAMP, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: 1, Nanos: 2000}}}},
			kind: "Timestamp", yql: "Timestamp(\"1970-01-01T00:00:01.000002Z\")"},
		{name: "interval", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INTERVAL, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Days: 1, Nanoseconds: 1000}}}},
			kind: "Interval", yql: "Interval(\"P1DT0.000001S\")"},
		{name: "optional int32", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT32, true, false), Value: signed(7)},
			kind: "Optional<Int32>", yql: "Just(7)"},
		{name: "null of optional utf8", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_STRING, true, false), Value: null},
			kind: "Optional<Utf8>", yql: "Nothing(Optional<Utf8>)"},
		{name: "list of uint64", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT64, false, true),
			Elements: []*protocol.DataValue{unsigned(1), unsigned(2)}}, kind: "List<Uint64>", yql: "[1ul,2ul]"},
		{name: "list of optional int16", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT16, true, true),
			Elements: []*protocol.DataValue{signed(1), null}}, kind: "List<Optional<Int16>>", yql: "[Just(1s),Nothing(Optional<Int16>)]"},
		{name: "empty list of utf8", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_STRING, false, true)},
			kind: "List<Utf8>", yql: "[]"},
		{name: "empty list of optional uint8", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT8, true, true)},
			kind: "List<Optional<Uint8>>", yql: "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := statementrows.TypedArgument(tc.argument)
			c.Assert(err, qt.IsNil)
			c.Assert(value.Type().Yql(), qt.Equals, tc.kind)
			c.Assert(value.Yql(), qt.Equals, tc.yql)
		})
	}
}

// A null of each scalar is a null of its own YDB type, so an optional column
// of any type takes it.
func TestTypedArgument_NullOfEachScalar(t *testing.T) {
	for _, tc := range []struct {
		scalar protocol.ArgumentType_Scalar
		kind   string
	}{
		{scalar: protocol.ArgumentType_BOOL, kind: "Optional<Bool>"},
		{scalar: protocol.ArgumentType_INT8, kind: "Optional<Int8>"},
		{scalar: protocol.ArgumentType_INT16, kind: "Optional<Int16>"},
		{scalar: protocol.ArgumentType_INT32, kind: "Optional<Int32>"},
		{scalar: protocol.ArgumentType_INT64, kind: "Optional<Int64>"},
		{scalar: protocol.ArgumentType_UINT8, kind: "Optional<Uint8>"},
		{scalar: protocol.ArgumentType_UINT16, kind: "Optional<Uint16>"},
		{scalar: protocol.ArgumentType_UINT32, kind: "Optional<Uint32>"},
		{scalar: protocol.ArgumentType_UINT64, kind: "Optional<Uint64>"},
		{scalar: protocol.ArgumentType_FLOAT32, kind: "Optional<Float>"},
		{scalar: protocol.ArgumentType_FLOAT64, kind: "Optional<Double>"},
		{scalar: protocol.ArgumentType_STRING, kind: "Optional<Utf8>"},
		{scalar: protocol.ArgumentType_BYTES, kind: "Optional<String>"},
		{scalar: protocol.ArgumentType_TIMESTAMP, kind: "Optional<Timestamp>"},
		{scalar: protocol.ArgumentType_INTERVAL, kind: "Optional<Interval>"},
	} {
		t.Run(tc.scalar.String(), func(t *testing.T) {
			c := testcheck.New(t)
			value, err := statementrows.TypedArgument(&protocol.StatementArgument{Type: argumentType(tc.scalar, true, false), Value: null})
			c.Assert(err, qt.IsNil)
			c.Assert(value.Type().Yql(), qt.Equals, tc.kind)
		})
	}
}

// A value its YDB type cannot hold, or one the SDK's validation would have
// refused, binds nothing.
func TestTypedArgument_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		argument *protocol.StatementArgument
		want     string
	}{
		{name: "an unspecified scalar", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_SCALAR_UNSPECIFIED, false, false), Value: signed(1)},
			want: "a typed statement argument names no YDB type for SCALAR_UNSPECIFIED"},
		{name: "a null its type does not admit", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT32, false, false), Value: null},
			want: "a typed statement argument is null only when its type admits null"},
		{name: "a null element its type does not admit", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT32, false, true),
			Elements: []*protocol.DataValue{null}}, want: "a typed statement argument is null only when its type admits null"},
		{name: "an int8 out of range", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT8, false, false), Value: signed(128)},
			want: "a typed statement argument's value is not one its scalar INT8 holds"},
		{name: "an int16 out of range", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT16, false, false), Value: signed(-32769)},
			want: "a typed statement argument's value is not one its scalar INT16 holds"},
		{name: "an int32 out of range", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT32, false, false), Value: signed(math.MaxInt32 + 1)},
			want: "a typed statement argument's value is not one its scalar INT32 holds"},
		{name: "a uint8 out of range", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT8, false, false), Value: unsigned(256)},
			want: "a typed statement argument's value is not one its scalar UINT8 holds"},
		{name: "a uint16 out of range", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT16, false, false), Value: unsigned(65536)},
			want: "a typed statement argument's value is not one its scalar UINT16 holds"},
		{name: "a uint32 out of range", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT32, false, false), Value: unsigned(math.MaxUint32 + 1)},
			want: "a typed statement argument's value is not one its scalar UINT32 holds"},
		{name: "a signed value for an unsigned scalar", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT64, false, false), Value: signed(0)},
			want: "a typed statement argument's value is not one its scalar UINT64 holds"},
		{name: "an unsigned value for a signed scalar", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT64, false, false), Value: unsigned(0)},
			want: "a typed statement argument's value is not one its scalar INT64 holds"},
		{name: "a double a float does not hold", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_FLOAT32, false, false), Value: double(0.1)},
			want: "a typed statement argument's value is not one its scalar FLOAT32 holds"},
		{name: "a double for an integer", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INT64, false, false), Value: double(1)},
			want: "a typed statement argument's value is not one its scalar INT64 holds"},
		{name: "text for bytes", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_BYTES, false, false), Value: text("CZ")},
			want: "a typed statement argument's value is not one its scalar BYTES holds"},
		{name: "bytes for text", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_STRING, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_BytesValue{}}}, want: "a typed statement argument's value is not one its scalar STRING holds"},
		{name: "text for a bool", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_BOOL, false, false), Value: text("true")},
			want: "a typed statement argument's value is not one its scalar BOOL holds"},
		{name: "a bool for text", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_STRING, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_BoolValue{}}}, want: "a typed statement argument's value is not one its scalar STRING holds"},
		{name: "an interval for a timestamp", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_TIMESTAMP, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{}}}},
			want: "a typed statement argument's value is not one its scalar TIMESTAMP holds"},
		{name: "a timestamp for an interval", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INTERVAL, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{}}}},
			want: "a typed statement argument's value is not one its scalar INTERVAL holds"},
		{name: "a timestamp finer than a microsecond", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_TIMESTAMP, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Nanos: 1}}}},
			want: "YDB timestamp has microsecond precision"},
		{name: "a timestamp before 1970", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_TIMESTAMP, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: -1}}}},
			want: "YDB timestamp is outside its range"},
		{name: "a timestamp from 2106", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_TIMESTAMP, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: 4291747200}}}},
			want: "YDB timestamp is outside its range"},
		{name: "an interval of months", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INTERVAL, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Months: 1}}}},
			want: "YDB interval requires fixed days and microsecond precision"},
		{name: "an interval finer than a microsecond", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INTERVAL, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Nanoseconds: 1}}}},
			want: "YDB interval requires fixed days and microsecond precision"},
		{name: "an interval outside its range", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_INTERVAL, false, false),
			Value: &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Days: math.MaxInt64}}}},
			want: "YDB interval is outside its range"},
		{name: "an element its scalar does not hold", argument: &protocol.StatementArgument{Type: argumentType(protocol.ArgumentType_UINT8, true, true),
			Elements: []*protocol.DataValue{unsigned(1), unsigned(256)}}, want: "a typed statement argument's value is not one its scalar UINT8 holds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := statementrows.TypedArgument(tc.argument)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(value, qt.IsNil)
		})
	}
}
