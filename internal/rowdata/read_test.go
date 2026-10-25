package rowdata_test

import (
	"encoding/binary"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
)

func int64Value(v int64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: v}}
}

func stringValue(v string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: v}}
}

func TestOrderAddsThePrimaryKeyTheRequestLeavesOut(t *testing.T) {
	c := qt.New(t)

	c.Assert(rowdata.Order([]string{"code"}, []string{"region", "code"}), qt.DeepEquals, []string{"code", "region"})
	c.Assert(rowdata.Order([]string{"id"}, []string{"id"}), qt.DeepEquals, []string{"id"})
}

func TestPageQueryText_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		query rowdata.PageQuery
		want  string
	}{
		{
			name:  "first page",
			query: rowdata.PageQuery{Table: "`app/regions`", Columns: []string{"name", "id"}, Order: []string{"id"}, OrderTypes: []string{"Int64"}, Limit: 101},
			want:  "SELECT `name`, `id` FROM `app/regions` ORDER BY `id` LIMIT 101",
		},
		{
			name: "after a composite key",
			query: rowdata.PageQuery{Table: "`t`", Columns: []string{"a", "b", "v"}, Order: []string{"a", "b"},
				OrderTypes: []string{"Utf8", "Int32"}, After: []*protocol.DataValue{stringValue("x'y"), int64Value(7)}, Limit: 2},
			want: "SELECT `a`, `b`, `v` FROM `t` WHERE (`a` > 'x\\'y'u) OR (`a` = 'x\\'y'u AND `b` > 7) ORDER BY `a`, `b` LIMIT 2",
		},
		{
			name: "after a null key",
			query: rowdata.PageQuery{Table: "`t`", Columns: []string{"k", "id"}, Order: []string{"k", "id"},
				OrderTypes: []string{"Utf8", "Uint64"}, After: []*protocol.DataValue{rowdata.Null(), {Value: &protocol.DataValue_Uint64Value{Uint64Value: 3}}}, Limit: 11},
			want: "SELECT `k`, `id` FROM `t` WHERE (`k` IS NOT NULL) OR (`k` IS NULL AND `id` > 3ul) ORDER BY `k`, `id` LIMIT 11",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			text, err := test.query.Text()

			c.Assert(err, qt.IsNil)
			c.Assert(text, qt.Equals, test.want)
		})
	}
}

func TestPageQueryText_FailurePath(t *testing.T) {
	c := qt.New(t)
	query := rowdata.PageQuery{Table: "`t`", Columns: []string{"id"}, Order: []string{"id"}, OrderTypes: []string{"Int8"},
		After: []*protocol.DataValue{int64Value(1000)}, Limit: 2}

	text, err := query.Text()

	c.Assert(err, qt.ErrorMatches, `Int8 cannot hold 1000: it is outside the range -128 to 127`)
	c.Assert(text, qt.Equals, "")
}

func TestPageToken_HappyPath(t *testing.T) {
	c := qt.New(t)
	values := []*protocol.DataValue{stringValue("north"), rowdata.Null(), int64Value(-4)}

	token, err := rowdata.EncodeToken([]string{"name", "region", "id"}, values)
	c.Assert(err, qt.IsNil)
	decoded, err := rowdata.DecodeToken(token, []string{"name", "region", "id"})

	c.Assert(err, qt.IsNil)
	c.Assert(decoded, qt.CmpEquals(protocmp.Transform()), values)
}

func TestPageToken_FailurePath(t *testing.T) {
	c := qt.New(t)
	token, err := rowdata.EncodeToken([]string{"id"}, []*protocol.DataValue{int64Value(1)})
	c.Assert(err, qt.IsNil)

	for _, test := range []struct{ name, token, want string }{
		{"another order", token, "the page token is not one this provider wrote for this read"},
		{"not base64", "%%%", "the page token is not one this provider wrote"},
		{"not a token", "e30", "the page token is not one this provider wrote for this read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			decoded, err := rowdata.DecodeToken(test.token, []string{"code"})

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(decoded, qt.IsNil)
		})
	}
}

func TestPageTokenRefusesAKeyTooLong_FailurePath(t *testing.T) {
	c := qt.New(t)
	long := make([]byte, 4096)

	token, err := rowdata.EncodeToken([]string{"id"}, []*protocol.DataValue{stringValue(string(long))})

	c.Assert(err, qt.ErrorMatches, "the key of the last row of the page is longer than a page token holds")
	c.Assert(token, qt.Equals, "")
}

// decimal is what the driver scans a Decimal column into.
type decimal struct {
	unscaled         int64
	precision, scale uint32
}

func (d decimal) Decimal() ([16]byte, uint32, uint32) {
	var bytes [16]byte
	binary.BigEndian.PutUint64(bytes[8:], uint64(d.unscaled))
	high := uint64(0)
	if d.unscaled < 0 {
		high = ^uint64(0)
	}
	binary.BigEndian.PutUint64(bytes[:8], high)
	return bytes, d.precision, d.scale
}

type uuidText string

func (u uuidText) String() string { return string(u) }

func TestScanned_HappyPath(t *testing.T) {
	zone := time.FixedZone("CEST", 7200)
	for _, test := range []struct {
		name    string
		ydbType string
		scanned any
		want    *protocol.DataValue
	}{
		{"null", "Utf8", nil, rowdata.Null()},
		{"int8", "Int8", int8(-5), int64Value(-5)},
		{"serial", "Serial", int32(5), int64Value(5)},
		{"uint8", "Uint8", uint8(250), &protocol.DataValue{Value: &protocol.DataValue_Uint64Value{Uint64Value: 250}}},
		{"float", "Float", float32(0.5), &protocol.DataValue{Value: &protocol.DataValue_DoubleValue{DoubleValue: 0.5}}},
		{"bool", "Bool", true, &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: true}}},
		{"decimal", "Decimal(10,2)", decimal{unscaled: 1250, precision: 10, scale: 2},
			&protocol.DataValue{Value: &protocol.DataValue_DecimalValue{DecimalValue: &protocol.DecimalValue{Digits: "1250", Precision: 10, Scale: 2}}}},
		{"negative decimal", "Decimal(22,9)", decimal{unscaled: -1, precision: 22, scale: 9},
			&protocol.DataValue{Value: &protocol.DataValue_DecimalValue{DecimalValue: &protocol.DecimalValue{Digits: "-1", Precision: 22, Scale: 9}}}},
		{"utf8", "Utf8", "été", stringValue("été")},
		{"uuid", "Uuid", uuidText("6f9619ff-8b86-d011-b42d-00c04fc964ff"), stringValue("6f9619ff-8b86-d011-b42d-00c04fc964ff")},
		{"dynumber", "DyNumber", ".15e1", stringValue(".15e1")},
		{"json", "Json", []byte(`{"a": 1}`), &protocol.DataValue{Value: &protocol.DataValue_JsonValue{JsonValue: `{"a": 1}`}}},
		{"string", "String", []byte{0, 255}, &protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: []byte{0, 255}}}},
		{"date", "Date", time.Date(2024, 10, 4, 2, 0, 0, 0, zone), &protocol.DataValue{Value: &protocol.DataValue_DateValue{DateValue: 20000}}},
		{"date32 before 1970", "Date32", time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), &protocol.DataValue{Value: &protocol.DataValue_DateValue{DateValue: -1}}},
		{"timestamp", "Timestamp", time.Date(2026, 1, 2, 5, 4, 5, 123456000, zone),
			&protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: 1767323045, Nanos: 123456000}}}},
		{"interval", "Interval", -time.Second, &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Nanoseconds: -1e9}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			value, err := rowdata.Scanned(test.ydbType, test.scanned)

			c.Assert(err, qt.IsNil)
			c.Assert(value, qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

func TestScanned_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, ydbType string
		scanned       any
		want          string
	}{
		{"text for an integer", "Int64", "5", `the driver returned a Go string for a Int64 column`},
		{"an integer out of range", "Int64", uint64(1 << 63), `the driver returned a Go uint64 for a Int64 column`},
		{"an unknown type", "Optional<Utf8>", "x", `the driver returned a Go string for a Optional<Utf8> column`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			value, err := rowdata.Scanned(test.ydbType, test.scanned)

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(value, qt.IsNil)
		})
	}
}

func TestColumnType(t *testing.T) {
	c := qt.New(t)

	c.Assert(rowdata.ColumnType("Optional<Decimal(22, 9)>"), qt.Equals, "Decimal(22,9)")
	c.Assert(rowdata.ColumnType("Utf8"), qt.Equals, "Utf8")
}

func TestGoValue_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		value *protocol.DataValue
		want  string
	}{
		{"a month", &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Months: 1}}},
			"a YDB interval holds no month, which has no fixed length"},
		{"a native value", &protocol.DataValue{Value: &protocol.DataValue_NativeValue{NativeValue: &protocol.NativeValue{Type: "x"}}},
			"a native value is not a value YDB stores"},
		{"an interval out of range", &protocol.DataValue{Value: &protocol.DataValue_IntervalValue{IntervalValue: &protocol.IntervalValue{Days: 1 << 40}}},
			"the interval is out of range"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			value, carried, err := rowdata.GoValue(test.value)

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(carried, qt.IsTrue)
			c.Assert(value, qt.IsNil)
		})
	}
}

func TestDecimalString(t *testing.T) {
	c := qt.New(t)

	c.Assert(rowdata.DecimalString("1250", 2), qt.Equals, "12.50")
	c.Assert(rowdata.DecimalString("-5", 3), qt.Equals, "-0.005")
	c.Assert(rowdata.DecimalString("7", 0), qt.Equals, "7")
}
