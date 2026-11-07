package columnsemantics_test

import (
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func TestNativeDefaultRoundTrip(t *testing.T) {
	for _, test := range []struct{ kind, value, literal string }{
		{"Int8", "+05", "5t"}, {"Int16", "-31", "-31s"}, {"Int32", "7", "7"}, {"Int64", "8", "8l"},
		{"Uint8", "5", "5ut"}, {"Uint16", "6", "6us"}, {"Uint32", "7", "7u"}, {"Uint64", "8", "8ul"},
		{"Bool", "yes", "true"}, {"Float", "1.5", "Float('1.5')"}, {"Double", "1.5", "Double('1.5')"},
		{"Utf8", "", "''u"}, {"String", "a'b", "'a\\'b'"}, {"Utf8", "' a''b '::text", "' a\\'b 'u"},
		{"Json", "{}", "Json('{}')"}, {"JsonDocument", "{}", "JsonDocument('{}')"}, {"DyNumber", "1.5", "DyNumber('1.5')"},
		{"Uuid", "00112233-4455-6677-8899-aabbccddeeff", "Uuid('00112233-4455-6677-8899-aabbccddeeff')"},
		{"Date", "2026-01-02", "Date('2026-01-02')"}, {"Date32", "1960-01-02", "Date32('1960-01-02')"},
		{"Datetime", "2026-01-02T03:04:05Z", "Datetime('2026-01-02T03:04:05Z')"},
		{"Datetime64", "1960-01-02T03:04:05Z", "Datetime64('1960-01-02T03:04:05Z')"},
		{"Timestamp", "2026-01-02T03:04:05.123456Z", "Timestamp('2026-01-02T03:04:05.123456Z')"},
		{"Timestamp64", "1960-01-02T03:04:05.123456Z", "Timestamp64('1960-01-02T03:04:05.123456Z')"},
		{"Interval", "PT26H", "Interval('P1DT2H')"}, {"Interval64", "P1000000D", "Interval64('P1000000D')"}, {"Decimal(10,2)", "-001.50", "Decimal('-1.5', 10, 2)"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := columnsemantics.NativeDefault(&protocol.DeclaredColumn{Type: test.kind, DefaultSet: true, DefaultValue: test.value, Nullable: true}, test.kind, map[string]bool{"small_integer_defaults": true, "document_type_defaults": true})
			c.Assert(err, qt.IsNil)
			literal, err := columnsemantics.Literal(value)
			c.Assert(err, qt.IsNil)
			c.Assert(literal, qt.Equals, test.literal)
		})
	}
}

// defaultOf is a column declared with the literal default value.
func defaultOf(value string) *protocol.DeclaredColumn {
	return &protocol.DeclaredColumn{DefaultSet: true, DefaultValue: value}
}

// TestNativeDefaultRefusal refuses each default a native creation cannot
// store: an expression, a type whose default the target lacks, a Serial's
// default, and a value its type cannot hold. A value is refused where YDB
// answers `Invalid value` for it, and where YDB would store another value
// than the one declared: rounded, truncated or out of the type's range. The
// values its type cannot hold are ported from the host's
// TestLiteral_FailurePath.
func TestNativeDefaultRefusal(t *testing.T) {
	documents := map[string]bool{"document_type_defaults": true}
	for _, test := range []struct {
		name, kind string
		column     *protocol.DeclaredColumn
		caps       map[string]bool
		wantErr    string
	}{
		{"expression", "Int64", &protocol.DeclaredColumn{DefaultExpression: "1+2"}, nil, "expression defaults require expression_defaults, unavailable on this YDB target"},
		{"range", "Int8", defaultOf("128"), nil, `Int8 cannot hold "128": it is not a Int8 value`},
		{"missing small integer capability", "Int16", defaultOf("3"), nil, "a default of type Int16 requires small_integer_defaults"},
		{"missing document capability", "JsonDocument", defaultOf("{}"), nil, "a default of type JsonDocument requires document_type_defaults"},
		{"serial literal", "BigSerial", defaultOf("3"), nil, "a Serial column takes its default from its sequence"},
		{"negative unsigned", "Uint32", defaultOf("-1"), nil, `Uint32 cannot hold "-1": it is not a Uint32 value`},
		{"a word in an integer", "Int32", defaultOf("five"), nil, `Int32 cannot hold "five": it is not a Int32 value`},
		{"a word in a bool", "Bool", defaultOf("maybe"), nil, `Bool cannot hold "maybe": it is not a Bool value`},
		{"a word in a double", "Double", defaultOf("NaNa"), nil, `Double cannot hold "NaNa": it is not a Double value`},
		// Go's ParseFloat reads each of these three; YDB answers `Invalid value`.
		{"digit separators in a double", "Double", defaultOf("1_000"), nil, `Double cannot hold "1_000": it is not a Double value`},
		{"a hex float", "Double", defaultOf("0x1p-2"), nil, `Double cannot hold "0x1p-2": it is not a Double value`},
		{"not a number", "Double", defaultOf("NaN"), nil, `Double cannot hold "NaN": it is not a Double value`},
		{"infinity in a DyNumber", "DyNumber", defaultOf("Inf"), documents, `DyNumber cannot hold "Inf": it is not a DyNumber value`},
		{"a date before the narrow range", "Date", defaultOf("1969-12-31"), nil, `Date cannot hold "1969-12-31": it is not a Date value`},
		{"a date past the narrow range", "Date", defaultOf("2106-01-01"), nil, `Date cannot hold "2106-01-01": it is not a Date value`},
		{"an instant before the narrow range", "Timestamp", defaultOf("1960-01-01 00:00:00"), nil,
			`Timestamp cannot hold "1960-01-01 00:00:00": it is not a Timestamp value`},
		{"a datetime past the narrow range", "Datetime", defaultOf("2106-01-01T00:00:00Z"), nil,
			`Datetime cannot hold "2106-01-01T00:00:00Z": it is not a Datetime value`},
		{"a decimal that would round", "Decimal(10,2)", defaultOf("1.234"), nil, `Decimal(10,2) cannot hold "1.234": it is not a Decimal(10,2) value`},
		{"a decimal that overflows", "Decimal(4,2)", defaultOf("123.4"), nil, `Decimal(4,2) cannot hold "123.4": it is not a Decimal(4,2) value`},
		{"a malformed uuid", "Uuid", defaultOf("not-a-uuid"), nil, `Uuid cannot hold "not-a-uuid": it is not a Uuid value`},
		{"a malformed date", "Date", defaultOf("02/01/2026"), nil, `Date cannot hold "02/01/2026": it is not a Date value`},
		{"a datetime with a fraction", "Datetime", defaultOf("2026-01-02T03:04:05.5Z"), nil,
			`Datetime cannot hold "2026-01-02T03:04:05.5Z": it is not a Datetime value`},
		{"a timestamp with nanoseconds", "Timestamp", defaultOf("2026-01-02T03:04:05.123456789Z"), nil,
			`Timestamp cannot hold "2026-01-02T03:04:05.123456789Z": it is not a Timestamp value`},
		{"interval words", "Interval", defaultOf("1 day"), nil, "invalid ISO 8601 interval"},
		{"a bare P", "Interval", defaultOf("P"), nil, "invalid ISO 8601 interval"},
		// 15250285 weeks and 9223372036855 seconds are past the microseconds an
		// Int64 holds, the unit YDB stores an interval in.
		{"weeks past Int64 microseconds", "Interval64", defaultOf("P15250285W"), nil, "invalid ISO 8601 interval"},
		{"seconds past Int64 microseconds", "Interval64", defaultOf("PT9223372036855S"), nil, "invalid ISO 8601 interval"},
		// Ported from the host's TestRender_RefusesWhatYDBCannotHold_FailurePath.
		{"NULL on a NOT NULL column", "Utf8", defaultOf("NULL"), nil, "the column is NOT NULL and its default is NULL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := columnsemantics.NativeDefault(test.column, test.kind, test.caps)
			c.Assert(err, qt.ErrorMatches, regexp.QuoteMeta(test.wantErr))
			c.Assert(value, qt.IsNil)
		})
	}
}

// A column without a default, and a nullable one whose default is NULL, have
// no native default.
func TestNativeDefaultAbsence(t *testing.T) {
	for _, column := range []*protocol.DeclaredColumn{{}, {DefaultSet: true, DefaultValue: "NULL", Nullable: true}} {
		c := testcheck.New(t)
		value, err := columnsemantics.NativeDefault(column, "Int64", nil)
		c.Assert(err, qt.IsNil)
		c.Assert(value, qt.IsNil)
	}
}

func TestNativeLiteralRejectsWrongScalar(t *testing.T) {
	for _, value := range []*Ydb.Value{{}, {Value: &Ydb.Value_TextValue{TextValue: "5"}}, {Value: &Ydb.Value_Int32Value{Int32Value: 5}, Items: []*Ydb.Value{{}}}} {
		c := testcheck.New(t)
		literal, err := columnsemantics.Literal(&Ydb.TypedValue{Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_INT32}}, Value: value})
		c.Assert(err, qt.IsNotNil)
		c.Assert(literal, qt.Equals, "")
	}
}

func TestCanonicalLiteralPreservesTheScalar(t *testing.T) {
	c := testcheck.New(t)
	value, err := columnsemantics.NativeDefault(&protocol.DeclaredColumn{DefaultSet: true, DefaultValue: "5", Nullable: true}, "Int32", nil)
	c.Assert(err, qt.IsNil)
	original := proto.CloneOf(value)
	canonical, err := columnsemantics.CanonicalLiteral(value)
	c.Assert(err, qt.IsNil)
	c.Assert(canonical.Type.GetTypeId(), qt.Equals, Ydb.Type_INT32)
	c.Assert(canonical.Value.GetInt32Value(), qt.Equals, int32(5))
	c.Assert(proto.Equal(value, original), qt.IsTrue)
	nested := proto.CloneOf(value)
	nested.Value = &Ydb.Value{Value: &Ydb.Value_NestedValue{NestedValue: nested.Value}}
	equivalent, err := columnsemantics.CanonicalLiteral(nested)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(canonical, equivalent), qt.IsTrue)
	nested.Value.Items = []*Ydb.Value{{Value: &Ydb.Value_Int32Value{Int32Value: 9}}}
	rejected, err := columnsemantics.CanonicalLiteral(nested)
	c.Assert(err, qt.IsNotNil)
	c.Assert(rejected, qt.IsNil)
	text, err := columnsemantics.Literal(nested)
	c.Assert(err, qt.IsNotNil)
	c.Assert(text, qt.Equals, "")
}
