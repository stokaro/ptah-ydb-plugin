package ydbtype_test

import (
	"math"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// IntervalText writes a stored duration in the form an Interval literal
// takes it, which is how a default YDB keeps in microseconds is spelled:
// days, hours, minutes and seconds with the zero parts left out, PT0S for
// zero, and the most negative value without overflow. Ported from the host's
// TestIntervalText.
func TestIntervalText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		micros int64
		want   string
	}{
		{name: "zero", micros: 0, want: "PT0S"},
		{name: "a day and two hours", micros: 93_600_000_000, want: "P1DT2H"},
		{name: "a whole day", micros: 86_400_000_000, want: "P1D"},
		{name: "minus a second", micros: -1_000_000, want: "-PT1S"},
		{name: "a microsecond", micros: 1, want: "PT0.000001S"},
		{name: "minutes and fractional seconds", micros: 61_250_000, want: "PT1M1.25S"},
		{name: "the most negative value", micros: math.MinInt64, want: "-P106751991DT4H54.775808S"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbtype.IntervalText(tc.micros), qt.Equals, tc.want)
		})
	}
}

// Each literal a column's type is written with reads back as a value, and
// that value writes the same literal again in the column's type. A String is
// written with every byte outside printable ASCII as \xHH, so its newline
// reads back from \x0a; a Utf8 keeps the named escapes. Ported from the
// host's TestLiteralValue_ReadsBackWhatLiteralWrote.
func TestLiteralValue_ReadsBackWhatLiteralWrote(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ydbType   string
		literal   string
		wantValue string
	}{
		{name: "bool", ydbType: "Bool", literal: "false", wantValue: "false"},
		{name: "int8", ydbType: "Int8", literal: "-5t", wantValue: "-5"},
		{name: "int32", ydbType: "Int32", literal: "5", wantValue: "5"},
		{name: "uint64", ydbType: "Uint64", literal: "18446744073709551615ul", wantValue: "18446744073709551615"},
		{name: "float", ydbType: "Float", literal: "Float('1.5')", wantValue: "1.5"},
		{name: "double with an exponent", ydbType: "Double", literal: "Double('1e+21')", wantValue: "1e+21"},
		{name: "string with escapes", ydbType: "String", literal: `'a\'b\\c\x00\x0a'`, wantValue: "a'b\\c\x00\n"},
		{name: "utf8", ydbType: "Utf8", literal: "'héllo'u", wantValue: "héllo"},
		{name: "utf8 with control characters", ydbType: "Utf8", literal: `'l1\nl2\x01'u`, wantValue: "l1\nl2\x01"},
		{name: "json with a quote", ydbType: "Json", literal: `Json('{"a":"it\'s"}')`, wantValue: `{"a":"it's"}`},
		{name: "uuid", ydbType: "Uuid", literal: "Uuid('550e8400-e29b-41d4-a716-446655440000')", wantValue: "550e8400-e29b-41d4-a716-446655440000"},
		{name: "date32", ydbType: "Date32", literal: "Date32('1900-01-02')", wantValue: "1900-01-02"},
		{name: "timestamp", ydbType: "Timestamp", literal: "Timestamp('2026-01-02T03:04:05.123456Z')", wantValue: "2026-01-02T03:04:05.123456Z"},
		{name: "interval", ydbType: "Interval64", literal: "Interval64('-PT1S')", wantValue: "-PT1S"},
		{name: "decimal", ydbType: "Decimal(22,9)", literal: "Decimal('-1.5', 22, 9)", wantValue: "-1.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)

			value, ok := ydbtype.LiteralValue(tc.literal)
			c.Assert(ok, qt.IsTrue)
			c.Assert(value, qt.Equals, tc.wantValue)

			again, err := ydbtype.ValueLiteral(tc.ydbType, value)
			c.Assert(err, qt.IsNil)
			c.Assert(again, qt.Equals, tc.literal)
		})
	}
}

// Text that is not a literal a column's type is written with reads as no
// literal, which a caller then treats as an expression. Ported from the
// host's TestLiteralValue_FailurePath.
func TestLiteralValue_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		literal string
	}{
		{name: "a function call", literal: "CurrentUtcTimestamp()"},
		{name: "an unknown suffix", literal: "5x"},
		{name: "an unterminated string", literal: "'abc"},
		{name: "two strings", literal: "'a' || 'b'"},
		{name: "an unknown escape", literal: `'\q'`},
		{name: "a short hexadecimal escape", literal: `'\x1'`},
		{name: "a constructor of a type no literal is written in", literal: "TzDate('2026-01-02,UTC')"},
		{name: "a decimal without its precision", literal: "Decimal('1.5')"},
		{name: "a constructor around an expression", literal: "Float(1.5)"},
		{name: "an empty text", literal: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)

			value, ok := ydbtype.LiteralValue(tc.literal)

			c.Assert(ok, qt.IsFalse)
			c.Assert(value, qt.Equals, "")
		})
	}
}
