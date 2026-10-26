package columnsemantics

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Literal writes a typed default value as the canonical YQL literal of its
// type, as the host's embedded engine writes a default: `5`, `'a'u`,
// `Date('2026-01-02')`. A value of a type it has no literal for is an error.
func Literal(value *Ydb.TypedValue) (string, error) {
	canonical, err := CanonicalLiteral(value)
	if err != nil {
		return "", err
	}
	kind, scalar := canonical.Type, canonical.Value
	name, _, err := schema.ColumnType(kind)
	if err != nil {
		return "", err
	}
	text, err := literalText(kind, scalar)
	if err != nil {
		return "", err
	}
	return ydbtype.ValueLiteral(name, text)
}

// CanonicalLiteral validates a scalar default and removes only its optional
// wrapper. The same non-null default can be returned with or without that
// wrapper by the table and query services.
func CanonicalLiteral(value *Ydb.TypedValue) (*Ydb.TypedValue, error) {
	if err := schema.Known(value); err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	if result == nil {
		return nil, errors.New("a native default requires its typed value")
	}
	if optional := result.Type.GetOptionalType(); optional != nil {
		result.Type = optional.Item
		if nested := result.Value.GetNestedValue(); nested != nil {
			if !proto.Equal(result.Value, &Ydb.Value{Value: &Ydb.Value_NestedValue{NestedValue: nested}}) {
				return nil, errors.New("an optional default contains fields outside its scalar value")
			}
			result.Value = nested
		}
	}
	if _, isNull := result.Value.GetValue().(*Ydb.Value_NullFlagValue); isNull {
		return nil, errors.New("a NULL default has no literal")
	}
	if _, _, err := schema.ColumnType(result.Type); err != nil {
		return nil, err
	}
	if err := validateLiteralScalar(result.Type, result.Value); err != nil {
		return nil, err
	}
	return result, nil
}

// literalText is the text spelling of a value, which the column's type reads
// back into the same value.
func literalText(kind *Ydb.Type, value *Ydb.Value) (any, error) {
	if decimal := kind.GetDecimalType(); decimal != nil {
		return decimalText(value.GetLow_128(), value.GetHigh_128(), decimal.GetScale()), nil
	}
	switch kind.GetTypeId() {
	case Ydb.Type_UUID:
		var id [16]byte
		binary.LittleEndian.PutUint64(id[:8], value.GetLow_128())
		binary.LittleEndian.PutUint64(id[8:], value.GetHigh_128())
		return uuid.UUID(uuidEndian(id)).String(), nil
	case Ydb.Type_BOOL:
		return value.GetBoolValue(), nil
	case Ydb.Type_INT8, Ydb.Type_INT16, Ydb.Type_INT32:
		return int64(value.GetInt32Value()), nil
	case Ydb.Type_INT64:
		return value.GetInt64Value(), nil
	case Ydb.Type_UINT8, Ydb.Type_UINT16, Ydb.Type_UINT32:
		return uint64(value.GetUint32Value()), nil
	case Ydb.Type_UINT64:
		return value.GetUint64Value(), nil
	case Ydb.Type_FLOAT:
		return value.GetFloatValue(), nil
	case Ydb.Type_DOUBLE:
		return value.GetDoubleValue(), nil
	case Ydb.Type_UTF8, Ydb.Type_JSON, Ydb.Type_JSON_DOCUMENT, Ydb.Type_DYNUMBER:
		return value.GetTextValue(), nil
	case Ydb.Type_STRING, Ydb.Type_YSON:
		return value.GetBytesValue(), nil
	case Ydb.Type_DATE:
		return time.Unix(int64(value.GetUint32Value())*86400, 0).UTC(), nil
	case Ydb.Type_DATETIME:
		return time.Unix(int64(value.GetUint32Value()), 0).UTC(), nil
	case Ydb.Type_TIMESTAMP:
		micros := value.GetUint64Value()
		if micros > math.MaxInt64 {
			return nil, fmt.Errorf("native timestamp exceeds the supported range")
		}
		return time.UnixMicro(int64(micros)).UTC(), nil
	case Ydb.Type_DATE32:
		return time.Unix(int64(value.GetInt32Value())*86400, 0).UTC(), nil
	case Ydb.Type_DATETIME64:
		return time.Unix(value.GetInt64Value(), 0).UTC(), nil
	case Ydb.Type_TIMESTAMP64:
		return time.UnixMicro(value.GetInt64Value()).UTC(), nil
	case Ydb.Type_INTERVAL, Ydb.Type_INTERVAL64:
		return ydbtype.IntervalText(value.GetInt64Value()), nil
	default:
		return nil, fmt.Errorf("YQL has no literal of this type")
	}
}

// decimalText writes a Decimal's 128-bit two's-complement integer at scale as
// decimal text.
func decimalText(low, high uint64, scale uint32) string {
	integer := new(big.Int).Lsh(new(big.Int).SetUint64(high), 64)
	integer.Or(integer, new(big.Int).SetUint64(low))
	if high>>63 == 1 {
		integer.Sub(integer, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	digits := new(big.Int).Abs(integer).String()
	sign := ""
	if integer.Sign() < 0 {
		sign = "-"
	}
	if scale == 0 {
		return sign + digits
	}
	for len(digits) <= int(scale) {
		digits = "0" + digits
	}
	return sign + digits[:len(digits)-int(scale)] + "." + digits[len(digits)-int(scale):]
}

// A protobuf scalar carries its own case. Reading a different case with a Get
// method yields zero, which would silently change the declared default.
func validateLiteralScalar(kind *Ydb.Type, value *Ydb.Value) error {
	if value == nil {
		return fmt.Errorf("a native default requires a scalar value")
	}
	expected := literalScalarFields[kind.GetTypeId()]
	if kind.GetDecimalType() != nil {
		expected = "low_128"
	}
	message := value.ProtoReflect()
	actual := message.WhichOneof(message.Descriptor().Oneofs().ByName("value"))
	if expected == "" || actual == nil || actual.Name() != expected {
		return fmt.Errorf("native default scalar does not match its type")
	}
	var extra bool
	message.Range(func(field protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		if field.Name() != expected && (expected != "low_128" || field.Name() != "high_128") {
			extra = true
		}
		return !extra
	})
	if extra {
		return fmt.Errorf("native default contains non-scalar fields")
	}
	return nil
}

var literalScalarFields = map[Ydb.Type_PrimitiveTypeId]protoreflect.Name{
	Ydb.Type_BOOL: "bool_value", Ydb.Type_INT8: "int32_value", Ydb.Type_INT16: "int32_value", Ydb.Type_INT32: "int32_value", Ydb.Type_INT64: "int64_value",
	Ydb.Type_UINT8: "uint32_value", Ydb.Type_UINT16: "uint32_value", Ydb.Type_UINT32: "uint32_value", Ydb.Type_UINT64: "uint64_value",
	Ydb.Type_FLOAT: "float_value", Ydb.Type_DOUBLE: "double_value", Ydb.Type_UTF8: "text_value", Ydb.Type_JSON: "text_value", Ydb.Type_JSON_DOCUMENT: "text_value", Ydb.Type_DYNUMBER: "text_value",
	Ydb.Type_STRING: "bytes_value", Ydb.Type_YSON: "bytes_value", Ydb.Type_UUID: "low_128", Ydb.Type_DATE: "uint32_value", Ydb.Type_DATETIME: "uint32_value", Ydb.Type_TIMESTAMP: "uint64_value",
	Ydb.Type_DATE32: "int32_value", Ydb.Type_DATETIME64: "int64_value", Ydb.Type_TIMESTAMP64: "int64_value", Ydb.Type_INTERVAL: "int64_value", Ydb.Type_INTERVAL64: "int64_value",
}
