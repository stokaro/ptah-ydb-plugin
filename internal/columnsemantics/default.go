package columnsemantics

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
)

// NativeDefault compiles a common literal into the typed value native creation
// stores. An explicit NULL default is absent. Expressions are refused because
// YDB default declarations accept values, not expressions evaluated for each row.
func NativeDefault(column *protocol.DeclaredColumn, spelling string, caps map[string]bool) (*Ydb.TypedValue, error) {
	if column.DefaultExpression != "" {
		return nil, fmt.Errorf("expression defaults require expression_defaults, unavailable on this YDB target")
	}
	if !column.DefaultSet && column.DefaultValue == "" {
		return nil, nil
	}
	value, isNull := declaredValue(column.DefaultValue)
	if isNull {
		return nil, nil
	}
	switch spelling {
	case ydbtype.Int16, ydbtype.Uint16:
		if !caps["small_integer_defaults"] {
			return nil, fmt.Errorf("a default of type %s requires small_integer_defaults", spelling)
		}
	case ydbtype.JSONDocument, ydbtype.DyNumber:
		if !caps["document_type_defaults"] {
			return nil, fmt.Errorf("a default of type %s requires document_type_defaults", spelling)
		}
	case ydbtype.Serial, ydbtype.BigSerial, ydbtype.SmallSerial:
		return nil, fmt.Errorf("a Serial column takes its default from its sequence")
	}
	if spelling == ydbtype.Interval || spelling == ydbtype.Interval64 {
		micros, err := ydbtype.IntervalMicroseconds(value)
		if err != nil {
			return nil, err
		}
		kind, err := schema.StorageType(spelling, column.Nullable)
		if err != nil {
			return nil, err
		}
		return &Ydb.TypedValue{Type: kind, Value: &Ydb.Value{Value: &Ydb.Value_Int64Value{Int64Value: micros}}}, nil
	}
	canonical, err := ydbtype.CanonicalValue(spelling, value)
	if err != nil {
		return nil, err
	}
	kind, err := schema.StorageType(spelling, column.Nullable)
	if err != nil {
		return nil, err
	}
	held, err := nativeValue(spelling, canonical)
	if err != nil {
		return nil, err
	}
	return &Ydb.TypedValue{Type: kind, Value: held}, nil
}

func nativeValue(spelling string, value any) (*Ydb.Value, error) {
	switch typed := value.(type) {
	case int64:
		if spelling == ydbtype.Int64 {
			return &Ydb.Value{Value: &Ydb.Value_Int64Value{Int64Value: typed}}, nil
		}
		if typed < math.MinInt32 || typed > math.MaxInt32 {
			return nil, fmt.Errorf("native integer default exceeds its storage width")
		}
		return &Ydb.Value{Value: &Ydb.Value_Int32Value{Int32Value: int32(typed)}}, nil
	case uint64:
		if spelling == ydbtype.Uint64 {
			return &Ydb.Value{Value: &Ydb.Value_Uint64Value{Uint64Value: typed}}, nil
		}
		if typed > math.MaxUint32 {
			return nil, fmt.Errorf("native unsigned default exceeds its storage width")
		}
		return &Ydb.Value{Value: &Ydb.Value_Uint32Value{Uint32Value: uint32(typed)}}, nil
	case bool:
		return &Ydb.Value{Value: &Ydb.Value_BoolValue{BoolValue: typed}}, nil
	case float64:
		if spelling == ydbtype.Float {
			return &Ydb.Value{Value: &Ydb.Value_FloatValue{FloatValue: float32(typed)}}, nil
		}
		return &Ydb.Value{Value: &Ydb.Value_DoubleValue{DoubleValue: typed}}, nil
	case []byte:
		return &Ydb.Value{Value: &Ydb.Value_BytesValue{BytesValue: typed}}, nil
	case string:
		return nativeText(spelling, typed)
	case time.Time:
		return nativeTime(spelling, typed)
	default:
		return nil, fmt.Errorf("no native default value for %s", spelling)
	}
}

func nativeText(spelling, text string) (*Ydb.Value, error) {
	if _, scale, ok := ydbtype.DecimalArguments(spelling); ok {
		integer, fraction, _ := strings.Cut(text, ".")
		if len(fraction) > scale {
			return nil, fmt.Errorf("canonical Decimal exceeds its scale")
		}
		digits := integer + fraction + strings.Repeat("0", scale-len(fraction))
		n, ok := new(big.Int).SetString(digits, 10)
		if !ok {
			return nil, fmt.Errorf("invalid canonical Decimal")
		}
		if n.Sign() < 0 {
			n.Add(n, new(big.Int).Lsh(big.NewInt(1), 128))
		}
		low := n.Uint64()
		high := new(big.Int).Rsh(n, 64).Uint64()
		return &Ydb.Value{Value: &Ydb.Value_Low_128{Low_128: low}, High_128: high}, nil
	}
	if spelling == ydbtype.UUID {
		id, err := uuid.Parse(text)
		if err != nil {
			return nil, err
		}
		wire := uuidEndian(id)
		return &Ydb.Value{Value: &Ydb.Value_Low_128{Low_128: binary.LittleEndian.Uint64(wire[:8])}, High_128: binary.LittleEndian.Uint64(wire[8:])}, nil
	}
	return &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: text}}, nil
}

func nativeTime(spelling string, value time.Time) (*Ydb.Value, error) {
	number := value.Unix()
	switch spelling {
	case ydbtype.Date, ydbtype.Date32:
		number /= 86400
	case ydbtype.Timestamp, ydbtype.Timestamp64:
		number = value.UnixMicro()
	}
	switch spelling {
	case ydbtype.Date, ydbtype.Datetime:
		if number < 0 || number > math.MaxUint32 {
			return nil, fmt.Errorf("native date default exceeds its storage width")
		}
		return &Ydb.Value{Value: &Ydb.Value_Uint32Value{Uint32Value: uint32(number)}}, nil
	case ydbtype.Timestamp:
		if number < 0 {
			return nil, fmt.Errorf("native timestamp default precedes the epoch")
		}
		return &Ydb.Value{Value: &Ydb.Value_Uint64Value{Uint64Value: uint64(number)}}, nil
	case ydbtype.Date32:
		if number < math.MinInt32 || number > math.MaxInt32 {
			return nil, fmt.Errorf("native date default exceeds its storage width")
		}
		return &Ydb.Value{Value: &Ydb.Value_Int32Value{Int32Value: int32(number)}}, nil
	default:
		return &Ydb.Value{Value: &Ydb.Value_Int64Value{Int64Value: number}}, nil
	}
}

// YDB stores the first UUID fields in little-endian order; this permutation
// also converts the wire bytes back to the canonical UUID byte order.
func uuidEndian(id [16]byte) [16]byte {
	id[0], id[1], id[2], id[3] = id[3], id[2], id[1], id[0]
	id[4], id[5] = id[5], id[4]
	id[6], id[7] = id[7], id[6]
	return id
}
