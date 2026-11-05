package statementrows

import (
	"fmt"
	"math"
	"math/big"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-sdk/v3/types"
)

// maxTimestampSeconds is the first second a YDB Timestamp cannot hold:
// 2106-01-01T00:00:00Z.
const maxTimestampSeconds = 4291747200

// TypedArgument is the YDB parameter of a typed statement argument, at the
// type its caller bound it at: each scalar at its own YDB type, an optional
// type as Optional<T> and its null as a null of T, a list as List<T> of its
// elements, and an UNTYPED null as Void, as a plain null binds. An empty list
// keeps its element type. A value the YDB type cannot hold is refused before
// any statement runs: a timestamp outside the Timestamp range or finer than a
// microsecond, and an interval of months or finer than a microsecond. The
// request is expected to have passed provider.ValidateRunStatementRequest; an
// argument that would not have is refused too.
func TypedArgument(argument *protocol.StatementArgument) (types.Value, error) {
	kind := argument.GetType()
	if kind.GetScalar() == protocol.ArgumentType_UNTYPED {
		return types.VoidValue(), nil
	}
	scalar, err := scalarType(kind.GetScalar())
	if err != nil {
		return nil, err
	}
	if !kind.GetList() {
		return typedElement(kind, scalar, argument.GetValue())
	}
	element := scalar
	if kind.GetOptional() {
		element = types.Optional(scalar)
	}
	if len(argument.GetElements()) == 0 {
		return types.ZeroValue(types.List(element)), nil
	}
	items := make([]types.Value, len(argument.GetElements()))
	for i, value := range argument.GetElements() {
		if items[i], err = typedElement(kind, scalar, value); err != nil {
			return nil, err
		}
	}
	return types.ListValue(items...), nil
}

// typedElement is one value of a typed argument: Optional<T> when the type
// admits null, and a null of T for its null.
func typedElement(kind *protocol.ArgumentType, scalar types.Type, value *protocol.DataValue) (types.Value, error) {
	if value.GetNull() != nil {
		if !kind.GetOptional() {
			return nil, fmt.Errorf("a typed statement argument is null only when its type admits null")
		}
		return types.NullValue(scalar), nil
	}
	bound, err := scalarValue(kind.GetScalar(), value)
	if err != nil || !kind.GetOptional() {
		return bound, err
	}
	return types.OptionalValue(bound), nil
}

// scalarTypes is the YDB type of each scalar.
var scalarTypes = map[protocol.ArgumentType_Scalar]types.Type{
	protocol.ArgumentType_BOOL: types.TypeBool,
	protocol.ArgumentType_INT8: types.TypeInt8, protocol.ArgumentType_INT16: types.TypeInt16,
	protocol.ArgumentType_INT32: types.TypeInt32, protocol.ArgumentType_INT64: types.TypeInt64,
	protocol.ArgumentType_UINT8: types.TypeUint8, protocol.ArgumentType_UINT16: types.TypeUint16,
	protocol.ArgumentType_UINT32: types.TypeUint32, protocol.ArgumentType_UINT64: types.TypeUint64,
	protocol.ArgumentType_FLOAT32: types.TypeFloat, protocol.ArgumentType_FLOAT64: types.TypeDouble,
	protocol.ArgumentType_STRING: types.TypeText, protocol.ArgumentType_BYTES: types.TypeBytes,
	protocol.ArgumentType_TIMESTAMP: types.TypeTimestamp, protocol.ArgumentType_INTERVAL: types.TypeInterval,
}

func scalarType(scalar protocol.ArgumentType_Scalar) (types.Type, error) {
	kind, ok := scalarTypes[scalar]
	if !ok {
		return nil, fmt.Errorf("a typed statement argument names no YDB type for %s", scalar)
	}
	return kind, nil
}

// scalarValue is a value that is not null, at the YDB type of its scalar.
func scalarValue(scalar protocol.ArgumentType_Scalar, value *protocol.DataValue) (types.Value, error) {
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_BoolValue:
		if scalar == protocol.ArgumentType_BOOL {
			return types.BoolValue(v.BoolValue), nil
		}
	case *protocol.DataValue_Int64Value:
		return signedValue(scalar, v.Int64Value)
	case *protocol.DataValue_Uint64Value:
		return unsignedValue(scalar, v.Uint64Value)
	case *protocol.DataValue_DoubleValue:
		return floatValue(scalar, v.DoubleValue)
	case *protocol.DataValue_StringValue:
		if scalar == protocol.ArgumentType_STRING {
			return types.TextValue(v.StringValue), nil
		}
	case *protocol.DataValue_BytesValue:
		if scalar == protocol.ArgumentType_BYTES {
			return types.BytesValue(v.BytesValue), nil
		}
	case *protocol.DataValue_TimestampValue:
		if scalar == protocol.ArgumentType_TIMESTAMP {
			return timestampValue(v.TimestampValue)
		}
	case *protocol.DataValue_IntervalValue:
		if scalar == protocol.ArgumentType_INTERVAL {
			return intervalValue(v.IntervalValue)
		}
	}
	return nil, fmt.Errorf("a typed statement argument's value is not one its scalar %s holds", scalar)
}

func signedValue(scalar protocol.ArgumentType_Scalar, v int64) (types.Value, error) {
	switch {
	case scalar == protocol.ArgumentType_INT64:
		return types.Int64Value(v), nil
	case scalar == protocol.ArgumentType_INT32 && v >= math.MinInt32 && v <= math.MaxInt32:
		return types.Int32Value(int32(v)), nil
	case scalar == protocol.ArgumentType_INT16 && v >= math.MinInt16 && v <= math.MaxInt16:
		return types.Int16Value(int16(v)), nil
	case scalar == protocol.ArgumentType_INT8 && v >= math.MinInt8 && v <= math.MaxInt8:
		return types.Int8Value(int8(v)), nil
	}
	return nil, fmt.Errorf("a typed statement argument's value is not one its scalar %s holds", scalar)
}

func unsignedValue(scalar protocol.ArgumentType_Scalar, v uint64) (types.Value, error) {
	switch {
	case scalar == protocol.ArgumentType_UINT64:
		return types.Uint64Value(v), nil
	case scalar == protocol.ArgumentType_UINT32 && v <= math.MaxUint32:
		return types.Uint32Value(uint32(v)), nil
	case scalar == protocol.ArgumentType_UINT16 && v <= math.MaxUint16:
		return types.Uint16Value(uint16(v)), nil
	case scalar == protocol.ArgumentType_UINT8 && v <= math.MaxUint8:
		return types.Uint8Value(uint8(v)), nil
	}
	return nil, fmt.Errorf("a typed statement argument's value is not one its scalar %s holds", scalar)
}

func floatValue(scalar protocol.ArgumentType_Scalar, v float64) (types.Value, error) {
	switch {
	case scalar == protocol.ArgumentType_FLOAT64:
		return types.DoubleValue(v), nil
	case scalar == protocol.ArgumentType_FLOAT32 && (float64(float32(v)) == v || math.IsNaN(v)):
		return types.FloatValue(float32(v)), nil
	}
	return nil, fmt.Errorf("a typed statement argument's value is not one its scalar %s holds", scalar)
}

// timestampValue is a Timestamp, which holds microseconds from 1970 to 2106.
// A typed argument keeps its type, so a moment outside that range is refused
// rather than bound as a Timestamp64.
func timestampValue(timestamp *protocol.TimestampValue) (types.Value, error) {
	if timestamp.GetNanos()%1000 != 0 {
		return nil, fmt.Errorf("YDB timestamp has microsecond precision")
	}
	if timestamp.GetSeconds() < 0 || timestamp.GetSeconds() >= maxTimestampSeconds {
		return nil, fmt.Errorf("YDB timestamp is outside its range")
	}
	// #nosec G115 -- the seconds are checked above, and the SDK's request validation holds the nanoseconds in [0, 1e9)
	return types.TimestampValue(uint64(timestamp.GetSeconds())*1000000 + uint64(timestamp.GetNanos()/1000)), nil
}

// intervalValue is an Interval of microseconds, as a plain interval binds.
func intervalValue(interval *protocol.IntervalValue) (types.Value, error) {
	if interval.GetMonths() != 0 || interval.GetNanoseconds()%1000 != 0 {
		return nil, fmt.Errorf("YDB interval requires fixed days and microsecond precision")
	}
	micros := new(big.Int).Mul(big.NewInt(interval.GetDays()), big.NewInt(86400000000))
	micros.Add(micros, big.NewInt(interval.GetNanoseconds()/1000))
	if !micros.IsInt64() {
		return nil, fmt.Errorf("YDB interval is outside its range")
	}
	return types.IntervalValueFromMicroseconds(micros.Int64()), nil
}
