package statementrows_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/statementrows"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-sdk/v3/types"
)

func TestArgumentRetainsType(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value *protocol.DataValue
		kind  string
	}{
		{name: "decimal", value: &protocol.DataValue{Value: &protocol.DataValue_DecimalValue{DecimalValue: &protocol.DecimalValue{Digits: "1250", Precision: 8, Scale: 2}}}, kind: "Decimal(8,2)"},
		{name: "date", value: &protocol.DataValue{Value: &protocol.DataValue_DateValue{DateValue: 1}}, kind: "Date"},
		{name: "timestamp", value: &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: 1}}}, kind: "Timestamp"},
		{name: "json", value: &protocol.DataValue{Value: &protocol.DataValue_JsonValue{JsonValue: "{}"}}, kind: "Json"},
		{name: "interval", value: &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Days: 1, Nanoseconds: 1000}}}, kind: "Interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			value, err := statementrows.Argument(tc.value)
			c.Assert(err, qt.IsNil)
			typed, ok := value.(types.Value)
			c.Assert(ok, qt.IsTrue)
			c.Assert(typed.Type().Yql(), qt.Equals, tc.kind)
		})
	}
}

func TestArgumentRefusesLossOfPrecision(t *testing.T) {
	c := qt.New(t)
	for _, argument := range []*protocol.DataValue{
		{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Nanos: 1}}},
		{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Months: 1}}},
		{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Nanoseconds: 1}}},
	} {
		value, err := statementrows.Argument(argument)
		c.Assert(err, qt.IsNotNil)
		c.Assert(value, qt.IsNil)
	}
}
