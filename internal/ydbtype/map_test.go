package ydbtype_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// The capabilities Map reads, as the newest and the oldest YDB line the host
// measured hold them: 26.2 has the 64-bit date and time types and a Decimal
// of any precision, and 25.1 has neither. Both have Serial columns.
var (
	newestLine = ydbtype.Capabilities{ydbtype.WideDateTimeTypes: true, ydbtype.ParameterizedDecimal: true, ydbtype.SerialColumns: true}
	oldestLine = ydbtype.Capabilities{ydbtype.SerialColumns: true}
)

// Every declaration family maps on the newest line and on the oldest one.
// Each YDB type on the right was created as a column by the host on local-ydb
// 26.2.1.14 and read back under that name; the 25.1 column differs only where
// the line does. Ported from the host's TestMap_HappyPath.
func TestMap_HappyPath(t *testing.T) {
	vector := "YDB stores a vector as bytes; its vector index keeps the dimension"
	for _, tc := range []struct {
		name     string
		declared string
		caps     ydbtype.Capabilities
		want     ydbtype.Mapping
	}{
		{name: "varchar keeps no length", declared: "VARCHAR(255)", caps: newestLine, want: ydbtype.Mapping{Type: "Utf8", Dropped: "length 255"}},
		{name: "varchar without a length", declared: "varchar", caps: newestLine, want: ydbtype.Mapping{Type: "Utf8"}},
		{name: "nvarchar max", declared: "NVARCHAR(MAX)", caps: newestLine, want: ydbtype.Mapping{Type: "Utf8"}},
		{name: "char keeps no length", declared: "CHAR(2)", caps: newestLine, want: ydbtype.Mapping{Type: "Utf8", Dropped: "length 2"}},
		{name: "text", declared: "TEXT", caps: newestLine, want: ydbtype.Mapping{Type: "Utf8"}},
		{name: "clob", declared: "CLOB", caps: newestLine, want: ydbtype.Mapping{Type: "Utf8"}},
		{name: "bytea", declared: "BYTEA", caps: newestLine, want: ydbtype.Mapping{Type: "String"}},
		{name: "varbinary keeps no length", declared: "VARBINARY(16)", caps: newestLine, want: ydbtype.Mapping{Type: "String", Dropped: "length 16"}},
		{name: "boolean", declared: "BOOLEAN", caps: newestLine, want: ydbtype.Mapping{Type: "Bool"}},
		{name: "tinyint", declared: "TINYINT", caps: newestLine, want: ydbtype.Mapping{Type: "Int8"}},
		{name: "smallint", declared: "smallint", caps: newestLine, want: ydbtype.Mapping{Type: "Int16"}},
		{name: "integer", declared: "INTEGER", caps: newestLine, want: ydbtype.Mapping{Type: "Int32"}},
		{name: "mysql display width", declared: "INT(11)", caps: newestLine, want: ydbtype.Mapping{Type: "Int32", Dropped: "display width 11"}},
		{name: "bigint", declared: "BIGINT", caps: newestLine, want: ydbtype.Mapping{Type: "Int64"}},
		{name: "postgres int8 is eight bytes", declared: "int8", caps: newestLine, want: ydbtype.Mapping{Type: "Int64"}},
		{name: "ydb Int8 is one byte", declared: "Int8", caps: newestLine, want: ydbtype.Mapping{Type: "Int8"}},
		{name: "unsigned bigint", declared: "BIGINT UNSIGNED", caps: newestLine, want: ydbtype.Mapping{Type: "Uint64"}},
		{name: "unsigned with extra space", declared: "int   unsigned", caps: newestLine, want: ydbtype.Mapping{Type: "Uint32"}},
		{name: "real", declared: "REAL", caps: newestLine, want: ydbtype.Mapping{Type: "Float"}},
		{name: "sql float is double", declared: "FLOAT", caps: newestLine, want: ydbtype.Mapping{Type: "Double"}},
		{name: "ydb Float is single", declared: "Float", caps: newestLine, want: ydbtype.Mapping{Type: "Float"}},
		{name: "float with single precision", declared: "FLOAT(24)", caps: newestLine, want: ydbtype.Mapping{Type: "Float"}},
		{name: "double precision", declared: "DOUBLE PRECISION", caps: newestLine, want: ydbtype.Mapping{Type: "Double"}},
		{name: "decimal", declared: "DECIMAL(10,2)", caps: newestLine, want: ydbtype.Mapping{Type: "Decimal(10,2)"}},
		{name: "numeric without a scale", declared: "NUMERIC(12)", caps: newestLine, want: ydbtype.Mapping{Type: "Decimal(12,0)"}},
		{name: "widest decimal", declared: "Decimal(35, 35)", caps: newestLine, want: ydbtype.Mapping{Type: "Decimal(35,35)"}},
		{name: "the fixed decimal on 25.1", declared: "DECIMAL(22,9)", caps: oldestLine, want: ydbtype.Mapping{Type: "Decimal(22,9)"}},
		{name: "json", declared: "JSON", caps: newestLine, want: ydbtype.Mapping{Type: "Json"}},
		{name: "jsonb", declared: "JSONB", caps: newestLine, want: ydbtype.Mapping{Type: "JsonDocument"}},
		{name: "uuid", declared: "UUID", caps: newestLine, want: ydbtype.Mapping{Type: "Uuid"}},
		{name: "timestamp is wide where it can be", declared: "TIMESTAMP", caps: newestLine, want: ydbtype.Mapping{Type: "Timestamp64"}},
		{name: "timestamp is narrow on 25.1", declared: "TIMESTAMP", caps: oldestLine, want: ydbtype.Mapping{Type: "Timestamp"}},
		{name: "timestamptz", declared: "TIMESTAMPTZ", caps: newestLine, want: ydbtype.Mapping{Type: "Timestamp64"}},
		{name: "timestamp with time zone", declared: "timestamp with time zone", caps: newestLine, want: ydbtype.Mapping{Type: "Timestamp64"}},
		{name: "mysql datetime", declared: "DATETIME", caps: newestLine, want: ydbtype.Mapping{Type: "Timestamp64"}},
		{name: "microsecond precision drops nothing", declared: "TIMESTAMP(6)", caps: newestLine, want: ydbtype.Mapping{Type: "Timestamp64"}},
		{name: "millisecond precision is reported", declared: "TIMESTAMP(3)", caps: newestLine,
			want: ydbtype.Mapping{Type: "Timestamp64", Dropped: "precision 3 (YDB keeps microseconds)"}},
		{name: "date is wide where it can be", declared: "DATE", caps: newestLine, want: ydbtype.Mapping{Type: "Date32"}},
		{name: "date is narrow on 25.1", declared: "DATE", caps: oldestLine, want: ydbtype.Mapping{Type: "Date"}},
		{name: "interval", declared: "INTERVAL", caps: newestLine, want: ydbtype.Mapping{Type: "Interval64"}},
		{name: "interval is narrow on 25.1", declared: "INTERVAL", caps: oldestLine, want: ydbtype.Mapping{Type: "Interval"}},
		{name: "ydb narrow Timestamp", declared: "Timestamp", caps: newestLine, want: ydbtype.Mapping{Type: "Timestamp"}},
		{name: "ydb narrow Date", declared: "Date", caps: newestLine, want: ydbtype.Mapping{Type: "Date"}},
		{name: "ydb Datetime", declared: "Datetime", caps: newestLine, want: ydbtype.Mapping{Type: "Datetime"}},
		{name: "ydb Timestamp64 in any case", declared: "timestamp64", caps: newestLine, want: ydbtype.Mapping{Type: "Timestamp64"}},
		{name: "ydb Utf8 in any case", declared: "UTF8", caps: newestLine, want: ydbtype.Mapping{Type: "Utf8"}},
		{name: "ydb Uint64", declared: "Uint64", caps: newestLine, want: ydbtype.Mapping{Type: "Uint64"}},
		{name: "ydb DyNumber", declared: "DyNumber", caps: newestLine, want: ydbtype.Mapping{Type: "DyNumber"}},
		{name: "ydb Yson", declared: "Yson", caps: newestLine, want: ydbtype.Mapping{Type: "Yson"}},
		{name: "ydb String is bytes", declared: "String", caps: newestLine, want: ydbtype.Mapping{Type: "String"}},
		{name: "cockroachdb STRING is text", declared: "STRING", caps: newestLine, want: ydbtype.Mapping{Type: "Utf8"}},
		{name: "a lower-case string is text", declared: "string", caps: newestLine, want: ydbtype.Mapping{Type: "Utf8"}},
		{name: "serial", declared: "SERIAL", caps: newestLine, want: ydbtype.Mapping{Type: "Serial", Serial: true}},
		{name: "bigserial", declared: "BIGSERIAL", caps: oldestLine, want: ydbtype.Mapping{Type: "BigSerial", Serial: true}},
		{name: "serial2", declared: "serial2", caps: newestLine, want: ydbtype.Mapping{Type: "SmallSerial", Serial: true}},
		{name: "surrounding space", declared: "  BIGINT  ", caps: newestLine, want: ydbtype.Mapping{Type: "Int64"}},
		{name: "vector keeps its dimension apart", declared: "vector(3)", caps: newestLine,
			want: ydbtype.Mapping{Type: "String", Dimension: 3, Dropped: "dimension 3 (" + vector + ")"}},
		{name: "vector at the largest dimension", declared: "VECTOR(16384)", caps: oldestLine,
			want: ydbtype.Mapping{Type: "String", Dimension: 16384, Dropped: "dimension 16384 (" + vector + ")"}},
		{name: "vector without a dimension", declared: "VECTOR", caps: newestLine, want: ydbtype.Mapping{Type: "String"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbtype.Map(tc.declared, tc.caps)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

// Each refusal names what YDB lacks. One that a capability would lift names
// the capability; one no YDB line has a type for names none. Ported from the
// host's TestMap_FailurePath.
func TestMap_FailurePath(t *testing.T) {
	withoutSerial := ydbtype.Capabilities{ydbtype.WideDateTimeTypes: true, ydbtype.ParameterizedDecimal: true}
	for _, tc := range []struct {
		name     string
		declared string
		caps     ydbtype.Capabilities
		wantKey  string
		wantErr  string
	}{
		{name: "ydb Varchar is bytes", declared: "Varchar", caps: newestLine,
			wantErr: `Varchar has no YDB counterpart: YDB reads Varchar as String, a byte string; declare Utf8 \(or VARCHAR\) for text and String for bytes`},
		{name: "ydb Varchar with a length", declared: "Varchar(10)", caps: newestLine, wantErr: `Varchar\(10\) has no YDB counterpart: .*`},
		{name: "time of day", declared: "TIME", caps: newestLine, wantErr: `TIME has no YDB counterpart: YDB has no time-of-day type`},
		{name: "timetz", declared: "timetz", caps: newestLine, wantErr: `timetz has no YDB counterpart: YDB has no time-of-day type`},
		{name: "tz type", declared: "TzTimestamp", caps: newestLine, wantErr: `TzTimestamp has no YDB counterpart: .*not supported by storage.*`},
		{name: "postgres array", declared: "TEXT[]", caps: newestLine, wantErr: `TEXT\[\] has no YDB counterpart: YDB has no array column type`},
		{name: "yql list", declared: "List<Int32>", caps: newestLine, wantErr: `List<Int32> has no YDB counterpart: YDB stores only primitive types.*`},
		{name: "optional spelling", declared: "Int32?", caps: newestLine, wantErr: `Int32\? has no YDB counterpart: nullability is declared with NOT NULL.*`},
		{name: "mysql enum", declared: "ENUM('a','b')", caps: newestLine, wantErr: `ENUM\('a','b'\) has no YDB counterpart: YDB has no enum type`},
		{name: "xml", declared: "XML", caps: newestLine, wantErr: `XML has no YDB counterpart: YDB has no XML type`},
		{name: "range", declared: "tstzrange", caps: newestLine, wantErr: `tstzrange has no YDB counterpart: YDB has no range type`},
		{name: "money", declared: "MONEY", caps: newestLine, wantErr: `MONEY has no YDB counterpart: YDB has no money type; declare a DECIMAL`},
		{name: "network", declared: "INET", caps: newestLine, wantErr: `INET has no YDB counterpart: YDB has no network address type`},
		{name: "geometric", declared: "POINT", caps: newestLine, wantErr: `POINT has no YDB counterpart: YDB has no geometric type`},
		{name: "citext", declared: "CITEXT", caps: newestLine, wantErr: `CITEXT has no YDB counterpart: .*`},
		{name: "vector over the largest dimension", declared: "VECTOR(16385)", caps: oldestLine,
			wantErr: `VECTOR\(16385\) has no YDB counterpart: the dimension must be between 1 and 16384, the most a YDB vector index takes`},
		{name: "vector of no dimension", declared: "VECTOR(0)", caps: newestLine,
			wantErr: `VECTOR\(0\) has no YDB counterpart: the dimension must be between 1 and 16384, .*`},
		{name: "vector with an element format", declared: "VECTOR(3, FLOAT32)", caps: newestLine,
			wantErr: `VECTOR\(3, FLOAT32\) has no YDB counterpart: a YDB vector column takes a dimension only; the element type is the vector index's vector_type`},
		{name: "half-precision vector", declared: "halfvec(3)", caps: newestLine,
			wantErr: `halfvec\(3\) has no YDB counterpart: YDB has no half-precision vector; .*declare VECTOR\(n\)`},
		{name: "sparse vector", declared: "SPARSEVEC(1000)", caps: newestLine,
			wantErr: `SPARSEVEC\(1000\) has no YDB counterpart: YDB has no sparse vector; .*declare VECTOR\(n\)`},
		{name: "postgres type in ydb", declared: "pgint4", caps: newestLine, wantErr: `pgint4 has no YDB counterpart: .*EnableTablePgTypes.*`},
		{name: "unknown name", declared: "geography_point", caps: newestLine, wantErr: `geography_point has no YDB counterpart: YDB has no type of that name`},
		{name: "mysql auto increment in the type", declared: "INT AUTO_INCREMENT", caps: newestLine, wantErr: `INT AUTO_INCREMENT has no YDB counterpart: .*`},
		{name: "decimal without a precision", declared: "DECIMAL", caps: newestLine, wantErr: `DECIMAL has no YDB counterpart: a YDB Decimal needs a precision and a scale.*`},
		{name: "decimal too wide", declared: "DECIMAL(36,2)", caps: newestLine, wantErr: `DECIMAL\(36,2\) has no YDB counterpart: the precision must be between 1 and 35`},
		{name: "decimal scale above precision", declared: "DECIMAL(5,6)", caps: newestLine, wantErr: `DECIMAL\(5,6\) has no YDB counterpart: the scale must be between 0 and the precision`},
		{name: "a length on a type without one", declared: "BOOLEAN(1)", caps: newestLine, wantErr: `BOOLEAN\(1\) has no YDB counterpart: Bool takes no arguments`},
		{name: "unclosed argument list", declared: "VARCHAR(10", caps: newestLine, wantErr: `VARCHAR\(10 has no YDB counterpart: the argument list does not close`},
		{name: "empty", declared: " ", caps: newestLine, wantErr: `an empty type has no YDB counterpart: a column needs a type`},
		{name: "decimal precision on 25.1", declared: "DECIMAL(10,2)", caps: oldestLine, wantKey: ydbtype.ParameterizedDecimal,
			wantErr: `DECIMAL\(10,2\) requires target capability parameterized_decimal: this line has Decimal\(22,9\) only`},
		{name: "wide native on 25.1", declared: "Date32", caps: oldestLine, wantKey: ydbtype.WideDateTimeTypes,
			wantErr: `Date32 requires target capability wide_date_time_types: .*`},
		{name: "serial without the key", declared: "SERIAL", caps: withoutSerial, wantKey: ydbtype.SerialColumns,
			wantErr: `SERIAL requires target capability serial_columns: this target has no Serial column type`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbtype.Map(tc.declared, tc.caps)
			c.Assert(err, qt.ErrorMatches, tc.wantErr)
			var refusal *ydbtype.Refusal
			c.Assert(err, qt.ErrorAs, &refusal)
			c.Assert(refusal.Key, qt.Equals, tc.wantKey)
			c.Assert(got, qt.Equals, ydbtype.Mapping{})
		})
	}
}

// A declaration lands on one YDB type per line, and the two lines differ
// only for the date and time declarations that follow the 64-bit types: a
// table built on 25.1 holds Timestamp where one built on 26.2 holds
// Timestamp64 for the same declaration. A vector is a String on both, its
// dimension the index's to keep. Ported from the host's TestRenderings; the
// comparison that reads both as the declaration is in
// internal/columnsemantics.
func TestMap_RenderingsOnEachLine(t *testing.T) {
	narrow := ydbtype.Capabilities{ydbtype.ParameterizedDecimal: true, ydbtype.SerialColumns: true}
	for _, tc := range []struct {
		declared     string
		wide, narrow string
	}{
		{declared: "TIMESTAMP", wide: "Timestamp64", narrow: "Timestamp"},
		{declared: "DATE", wide: "Date32", narrow: "Date"},
		{declared: "INTERVAL", wide: "Interval64", narrow: "Interval"},
		{declared: "VARCHAR(100)", wide: "Utf8", narrow: "Utf8"},
		{declared: "Timestamp", wide: "Timestamp", narrow: "Timestamp"},
		{declared: "DECIMAL(10,2)", wide: "Decimal(10,2)", narrow: "Decimal(10,2)"},
		{declared: "vector(1536)", wide: "String", narrow: "String"},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			c := testcheck.New(t)
			wide, err := ydbtype.Map(tc.declared, newestLine)
			c.Assert(err, qt.IsNil)
			c.Assert(wide.Type, qt.Equals, tc.wide)
			onNarrow, err := ydbtype.Map(tc.declared, narrow)
			c.Assert(err, qt.IsNil)
			c.Assert(onNarrow.Type, qt.Equals, tc.narrow)
		})
	}
}
