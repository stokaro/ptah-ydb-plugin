package ydbtype_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// Each Go type is the one ydb-go-sdk's database/sql driver scanned a value of
// the YDB type into, measured by the host on local-ydb 26.2.1.14, except
// where the map's doc comment says why it differs. Ported from the host's
// TestGoTypeOf_HappyPath.
func TestGoTypeOf_HappyPath(t *testing.T) {
	decimal := ydbtype.GoType{Name: "types.Decimal", Import: "github.com/ydb-platform/ydb-go-sdk/v3/table/types"}
	instant := ydbtype.GoType{Name: "time.Time", Import: "time"}
	duration := ydbtype.GoType{Name: "time.Duration", Import: "time"}
	for _, tc := range []struct {
		name    string
		ydbType string
		want    ydbtype.GoType
	}{
		{name: "bool", ydbType: "Bool", want: ydbtype.GoType{Name: "bool"}},
		{name: "one byte integer", ydbType: "Int8", want: ydbtype.GoType{Name: "int8"}},
		{name: "two byte integer", ydbType: "Int16", want: ydbtype.GoType{Name: "int16"}},
		{name: "four byte integer", ydbType: "Int32", want: ydbtype.GoType{Name: "int32"}},
		{name: "eight byte integer", ydbType: "Int64", want: ydbtype.GoType{Name: "int64"}},
		{name: "one byte unsigned", ydbType: "Uint8", want: ydbtype.GoType{Name: "uint8"}},
		{name: "two byte unsigned", ydbType: "Uint16", want: ydbtype.GoType{Name: "uint16"}},
		{name: "four byte unsigned", ydbType: "Uint32", want: ydbtype.GoType{Name: "uint32"}},
		{name: "eight byte unsigned", ydbType: "Uint64", want: ydbtype.GoType{Name: "uint64"}},
		{name: "single precision", ydbType: "Float", want: ydbtype.GoType{Name: "float32"}},
		{name: "double precision", ydbType: "Double", want: ydbtype.GoType{Name: "float64"}},
		{name: "dynumber", ydbType: "DyNumber", want: ydbtype.GoType{Name: "string"}},
		{name: "bytes", ydbType: "String", want: ydbtype.GoType{Name: "[]byte"}},
		{name: "text", ydbType: "Utf8", want: ydbtype.GoType{Name: "string"}},
		{name: "json", ydbType: "Json", want: ydbtype.GoType{Name: "string"}},
		{name: "json document", ydbType: "JsonDocument", want: ydbtype.GoType{Name: "string"}},
		{name: "yson", ydbType: "Yson", want: ydbtype.GoType{Name: "[]byte"}},
		{name: "uuid", ydbType: "Uuid", want: ydbtype.GoType{Name: "string"}},
		{name: "date", ydbType: "Date", want: instant},
		{name: "datetime", ydbType: "Datetime", want: instant},
		{name: "timestamp", ydbType: "Timestamp", want: instant},
		{name: "wide date", ydbType: "Date32", want: instant},
		{name: "wide datetime", ydbType: "Datetime64", want: instant},
		{name: "wide timestamp", ydbType: "Timestamp64", want: instant},
		{name: "interval", ydbType: "Interval", want: duration},
		{name: "wide interval", ydbType: "Interval64", want: duration},
		{name: "small serial", ydbType: "SmallSerial", want: ydbtype.GoType{Name: "int16"}},
		{name: "serial", ydbType: "Serial", want: ydbtype.GoType{Name: "int32"}},
		{name: "big serial", ydbType: "BigSerial", want: ydbtype.GoType{Name: "int64"}},
		{name: "fixed decimal", ydbType: "Decimal(22,9)", want: decimal},
		{name: "decimal with spaces", ydbType: " Decimal(35, 10) ", want: decimal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, ok := ydbtype.GoTypeOf(tc.ydbType)
			c.Assert(ok, qt.IsTrue)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

// A name that is not a YDB type has no answer. A SQL declaration is one of
// them: Map reads it first, and only its YDB answer has a Go type here.
// Ported from the host's TestGoTypeOf_FailurePath.
func TestGoTypeOf_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ydbType string
	}{
		{name: "sql declaration", ydbType: "VARCHAR(255)"},
		{name: "postgres eight byte integer", ydbType: "int8"},
		{name: "sql float", ydbType: "FLOAT"},
		{name: "varchar is bytes on YDB and never written", ydbType: "Varchar"},
		{name: "unclosed decimal", ydbType: "Decimal(22,9"},
		{name: "empty", ydbType: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, ok := ydbtype.GoTypeOf(tc.ydbType)
			c.Assert(ok, qt.IsFalse)
			c.Assert(got, qt.Equals, ydbtype.GoType{})
		})
	}
}

// Every YDB type Map writes for a declaration has a Go type, on a line with
// the 64-bit date and time types and on one without, so a column a read
// reports never falls through to a guess. Ported from the host's
// TestGoTypeOf_CoversEveryRendering.
func TestGoTypeOf_CoversEveryRendering(t *testing.T) {
	everyLine := []string{
		"BOOLEAN", "TINYINT", "SMALLINT", "INTEGER", "BIGINT", "TINYINT UNSIGNED", "SMALLINT UNSIGNED",
		"INT UNSIGNED", "BIGINT UNSIGNED", "REAL", "DOUBLE PRECISION", "DYNUMBER", "TEXT", "BYTEA",
		"JSON", "JSONB", "YSON", "UUID", "DATE", "TIMESTAMP", "INTERVAL", "DECIMAL(10,2)",
		"SERIAL", "BIGSERIAL", "SMALLSERIAL", "Datetime",
	}
	for _, line := range []struct {
		name         string
		caps         ydbtype.Capabilities
		declarations []string
	}{
		{name: "wide", caps: ydbtype.Capabilities{ydbtype.WideDateTimeTypes: true, ydbtype.ParameterizedDecimal: true, ydbtype.SerialColumns: true},
			declarations: append([]string{"DATETIME64"}, everyLine...)},
		{name: "narrow", caps: ydbtype.Capabilities{ydbtype.ParameterizedDecimal: true, ydbtype.SerialColumns: true},
			declarations: everyLine},
	} {
		for _, declared := range line.declarations {
			t.Run(line.name+"/"+declared, func(t *testing.T) {
				c := testcheck.New(t)
				mapping, err := ydbtype.Map(declared, line.caps)
				c.Assert(err, qt.IsNil)
				_, ok := ydbtype.GoTypeOf(mapping.Type)
				c.Assert(ok, qt.IsTrue, qt.Commentf("%s renders as %s", declared, mapping.Type))
			})
		}
	}
}
