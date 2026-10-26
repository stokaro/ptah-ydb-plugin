package statementrows

import (
	"fmt"
	"math"
	"math/big"

	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-sdk/v3/types"
)

// Argument preserves the neutral value's type in a YDB parameter. Values
// beyond the engine's precision are refused before any statement runs.
func Argument(argument *protocol.DataValue) (any, error) {
	switch value := argument.GetValue().(type) {
	case *protocol.DataValue_JsonValue:
		return types.JSONValue(value.JsonValue), nil
	case *protocol.DataValue_DecimalValue:
		decimal := value.DecimalValue
		if decimal.GetPrecision() > 35 {
			return nil, fmt.Errorf("YDB decimal precision exceeds 35")
		}
		return types.DecimalValueFromString(rowdata.DecimalString(decimal.GetDigits(), decimal.GetScale()), decimal.GetPrecision(), decimal.GetScale())
	case *protocol.DataValue_DateValue:
		if value.DateValue < math.MinInt32 || value.DateValue > math.MaxInt32 {
			return nil, fmt.Errorf("YDB date is outside its range")
		}
		if value.DateValue >= 0 && value.DateValue < 49673 {
			return types.DateValue(uint32(value.DateValue)), nil
		}
		return types.Date32Value(int32(value.DateValue)), nil
	case *protocol.DataValue_TimestampValue:
		timestamp := value.TimestampValue
		if timestamp.GetNanos()%1000 != 0 {
			return nil, fmt.Errorf("YDB timestamp has microsecond precision")
		}
		micros := new(big.Int).Mul(big.NewInt(timestamp.GetSeconds()), big.NewInt(1000000))
		micros.Add(micros, big.NewInt(int64(timestamp.GetNanos()/1000)))
		if !micros.IsInt64() {
			return nil, fmt.Errorf("YDB timestamp is outside its range")
		}
		if micros.Sign() >= 0 && timestamp.GetSeconds() < 4291747200 {
			return types.TimestampValue(micros.Uint64()), nil
		}
		return types.Timestamp64Value(micros.Int64()), nil
	case *protocol.DataValue_IntervalValue:
		interval := value.IntervalValue
		if interval.GetMonths() != 0 || interval.GetNanoseconds()%1000 != 0 {
			return nil, fmt.Errorf("YDB interval requires fixed days and microsecond precision")
		}
		micros := new(big.Int).Mul(big.NewInt(interval.GetDays()), big.NewInt(86400000000))
		micros.Add(micros, big.NewInt(interval.GetNanoseconds()/1000))
		if !micros.IsInt64() {
			return nil, fmt.Errorf("YDB interval is outside its range")
		}
		return types.IntervalValueFromMicroseconds(micros.Int64()), nil
	default:
		converted, carried, err := rowdata.GoValue(argument)
		if err != nil {
			return nil, err
		}
		if !carried {
			return nil, fmt.Errorf("statement argument is unset")
		}
		return converted, nil
	}
}
