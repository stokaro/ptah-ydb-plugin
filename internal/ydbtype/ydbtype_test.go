package ydbtype_test

import (
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// line251 is what YDB 25.1 states of the keys the map reads: neither a Decimal
// of any precision nor the 64-bit date and time types.
var line251 = ydbtype.Capabilities{ydbtype.SerialColumns: true}

// line252 is what YDB 25.2 and every later line state of those keys.
var line252 = ydbtype.Capabilities{ydbtype.SerialColumns: true, ydbtype.ParameterizedDecimal: true, ydbtype.WideDateTimeTypes: true}

// A Decimal of a chosen precision and a 64-bit date type are refused on 25.1
// by the key that names what the line lacks. Ported from the host's
// TestRender_RefusesByCapability_FailurePath, its 25.1 rows.
func TestMap_RefusesByCapability_FailurePath(t *testing.T) {
	tests := []struct {
		declared, key, wantErr string
	}{
		{declared: "DECIMAL(10,2)", key: ydbtype.ParameterizedDecimal,
			wantErr: "DECIMAL(10,2) requires target capability parameterized_decimal: this line has Decimal(22,9) only"},
		{declared: "Date32", key: ydbtype.WideDateTimeTypes,
			wantErr: "Date32 requires target capability wide_date_time_types: the 64-bit date and time types are behind a flag that is off on this line"},
	}
	for _, test := range tests {
		t.Run(test.declared, func(t *testing.T) {
			c := testcheck.New(t)

			mapping, err := ydbtype.Map(test.declared, line251)

			c.Assert(err, qt.ErrorMatches, regexp.QuoteMeta(test.wantErr))
			var refusal *ydbtype.Refusal
			c.Assert(err, qt.ErrorAs, &refusal)
			c.Assert(refusal.Key, qt.Equals, test.key)
			c.Assert(mapping, qt.Equals, ydbtype.Mapping{})
		})
	}
}

// The controls: 25.1 keeps the one Decimal it has, and a line with both keys
// takes either declaration.
func TestMap_TakesWhatTheLineHas_HappyPath(t *testing.T) {
	tests := []struct {
		name, declared string
		caps           ydbtype.Capabilities
		want           string
	}{
		{name: "the Decimal of 25.1", declared: "DECIMAL(22,9)", caps: line251, want: "Decimal(22,9)"},
		{name: "a Decimal of a chosen precision on 25.2", declared: "DECIMAL(10,2)", caps: line252, want: "Decimal(10,2)"},
		{name: "a 64-bit date type on 25.2", declared: "Date32", caps: line252, want: "Date32"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			mapping, err := ydbtype.Map(test.declared, test.caps)

			c.Assert(err, qt.IsNil)
			c.Assert(mapping, qt.Equals, ydbtype.Mapping{Type: test.want})
		})
	}
}
