package ydbsequence_test

import (
	"math"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
)

// A Serial's sequence is named after the column, inside its table, and
// addressed by its absolute path.
func TestPath_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, database, schema, table, want string
	}{
		{name: "a nested table", database: "/local", schema: "app/orders", table: "items", want: "/local/app/orders/items/_serial_column_id"},
		{name: "a table at the root", database: "/local", table: "items", want: "/local/items/_serial_column_id"},
		{name: "a database without its slash", database: "local", schema: "app", table: "items", want: "/local/app/items/_serial_column_id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbsequence.Path(test.database, test.schema, test.table, "id"), qt.Equals, test.want)
		})
	}
}

func TestParse_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, start, increment string
		want                   ydbsequence.Settings
	}{
		{name: "nothing declared", want: ydbsequence.Default},
		{name: "a start and an increment", start: "100", increment: "5", want: ydbsequence.Settings{Start: 100, Increment: 5}},
		{name: "leading zeros and spaces", start: " 0100 ", increment: "05", want: ydbsequence.Settings{Start: 100, Increment: 5}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbsequence.Parse(test.start, test.increment)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

// A setting YDB refuses is refused with what YDB answers.
func TestParse_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, start, increment, want string
	}{
		{name: "a start of 0", start: "0", want: `identity_start 0: a YDB Serial's sequence takes a start of 1 or more .*`},
		{name: "a negative increment", increment: "-3", want: `identity_increment -3: a YDB Serial's sequence takes an increment of 1 or more .*`},
		{name: "a fraction", start: "1.5", want: `identity_start "1.5" is not a whole number YDB's ALTER SEQUENCE takes`},
		{name: "past the Int64 maximum", increment: "9223372036854775808", want: `identity_increment "9223372036854775808" is not a whole number .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbsequence.Parse(test.start, test.increment)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(got, qt.Equals, ydbsequence.Settings{})
		})
	}
}

// Canonical reads every spelling of a setting the way Parse does, and keeps a
// value Parse refuses as written.
func TestCanonical(t *testing.T) {
	for _, test := range []struct{ value, want string }{
		{"", "1"}, {" 1 ", "1"}, {"0100", "100"}, {"+7", "7"}, {"ten", "ten"},
	} {
		t.Run(test.value, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbsequence.Canonical(test.value), qt.Equals, test.want)
		})
	}
}

// Each Serial type has the integer it fills and the maximum YQL gives its
// sequence; no other type has a Serial.
func TestSerialTypes(t *testing.T) {
	for _, test := range []struct {
		storage, serial string
		maximum         int64
	}{
		{"Int16", "SmallSerial", math.MaxInt16},
		{"Int32", "Serial", math.MaxInt32},
		{"Int64", "BigSerial", math.MaxInt64},
	} {
		t.Run(test.storage, func(t *testing.T) {
			c := testcheck.New(t)
			serial, ok := ydbsequence.SerialFor(test.storage)
			c.Assert(ok, qt.IsTrue)
			c.Assert(serial, qt.Equals, test.serial)
			c.Assert(ydbsequence.Maximum(serial), qt.Equals, test.maximum)
		})
	}
}

// YDB has no Serial of an unsigned or a non-integer type.
func TestSerialFor_FailurePath(t *testing.T) {
	for _, storage := range []string{"Uint64", "Int8", "Utf8"} {
		t.Run(storage, func(t *testing.T) {
			c := testcheck.New(t)
			serial, ok := ydbsequence.SerialFor(storage)
			c.Assert(ok, qt.IsFalse)
			c.Assert(serial, qt.Equals, "")
		})
	}
}

// Only a narrow Serial given settings other than the default, on a target
// whose ALTER SEQUENCE does not keep its range, is refused.
func TestRangeRefusal(t *testing.T) {
	five := ydbsequence.Settings{Start: 5, Increment: 1}
	for _, test := range []struct {
		name       string
		serial     string
		settings   ydbsequence.Settings
		keepsRange bool
		want       string
	}{
		{name: "a BigSerial", serial: "BigSerial", settings: five},
		{name: "the defaults", serial: "Serial", settings: ydbsequence.Default},
		{name: "a target that keeps the range", serial: "Serial", settings: five, keepsRange: true},
		{name: "a Serial", serial: "Serial", settings: five, want: `an ALTER SEQUENCE raises the maximum of a Serial's sequence from 2147483647 to the Int64 maximum, .*`},
		{name: "a SmallSerial", serial: "SmallSerial", settings: five, want: `an ALTER SEQUENCE raises the maximum of a SmallSerial's sequence from 32767 to the Int64 maximum, .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbsequence.RangeRefusal(test.serial, test.settings, test.keepsRange), qt.Matches, test.want)
		})
	}
}
