// Package ydbtype maps a declared column type to the YDB type that holds it,
// and writes a value a data statement stores as the typed YQL literal that
// type takes.
//
// It is the reading of the Ptah host's internal/ydbtype, which the host's
// embedded YDB engine types managed rows and writes their statements with. The
// row services of this provider answer what that engine answers, and the
// corpus in internal/rowdata/testdata compares the two on every case it holds.
//
// The host measured every mapping on ydbplatform/local-ydb 25.1.4.7 through
// 26.2.1.14. The answers that shape the map:
//
//   - YDB has no length-limited string: `Utf8(255)` and `String(10)` are parse
//     errors. A declared length is dropped and reported through
//     [Mapping.Dropped].
//   - `Varchar` is accepted and becomes String, a byte string, so a declared
//     VARCHAR maps to Utf8 and the YDB spelling `Varchar` is refused.
//   - `Decimal` with no arguments is a parse error, the precision tops out at
//     35, and 25.1 has Decimal(22,9) only (capability parameterized_decimal).
//   - The 64-bit date and time types exist from 25.2 (capability
//     wide_date_time_types), and the Tz types are refused by storage on every
//     line.
//
// # Spelling
//
// A YDB type name that SQL also uses for a different type is read as YDB's
// only in YDB's own spelling. `Int8` is YDB's one-byte integer and `INT8` is
// PostgreSQL's eight-byte one; `Float` is YDB's single precision and `FLOAT` is
// SQL's double; `Date`, `Datetime`, `Timestamp` and `Interval` are YDB's narrow
// types where `DATE`, `DATETIME`, `TIMESTAMP` and `INTERVAL` follow
// wide_date_time_types. Every other YDB name (Utf8, Uint64, JsonDocument,
// DyNumber, Timestamp64, ...) is accepted in any case.
package ydbtype

import (
	"fmt"
	"strconv"
	"strings"
)

// Capabilities are the facts a host knows about its target, by the host's
// capability names. An absent key is a capability the target lacks, as the
// host reads it.
type Capabilities map[string]bool

// Has reports whether the target has the capability key.
func (c Capabilities) Has(key string) bool { return c[key] }

// The capability names the map reads.
const (
	// WideDateTimeTypes is the 64-bit date and time types.
	WideDateTimeTypes = "wide_date_time_types"
	// ParameterizedDecimal is a Decimal of any precision and scale.
	ParameterizedDecimal = "parameterized_decimal"
	// SerialColumns is the Serial column types.
	SerialColumns = "serial_columns"
)

// Mapping is the YDB type a declaration lands on.
type Mapping struct {
	// Type is the YDB spelling, as YDB prints it: Utf8, Int64, Decimal(10,2),
	// Serial.
	Type string
	// Dropped names what the declaration said that the YDB type does not keep,
	// such as `length 255` for VARCHAR(255); it is empty when nothing was
	// dropped.
	Dropped string
	// Serial reports a Serial, BigSerial or SmallSerial column, which YDB
	// fills from a sequence it creates with the table.
	Serial bool
	// Dimension is the dimension a VECTOR(n) declaration names, and zero
	// for every other declaration.
	Dimension uint64
}

// Refusal says why a declared type or value has no YDB counterpart on the
// target in hand.
type Refusal struct {
	// Declared is the type or value as the declaration wrote it.
	Declared string
	// Key is the capability whose absence refuses the declaration. It is
	// empty where no capability would change the answer.
	Key string
	// Reason says what YDB lacks, in a sentence a person can act on.
	Reason string
}

// Error renders the refusal as the host does.
func (r *Refusal) Error() string {
	if r.Key != "" {
		return fmt.Sprintf("%s requires target capability %s: %s", r.Declared, r.Key, r.Reason)
	}
	return fmt.Sprintf("%s has no YDB counterpart: %s", r.Declared, r.Reason)
}

// The YDB type names this package writes.
const (
	Bool         = "Bool"
	Int8         = "Int8"
	Int16        = "Int16"
	Int32        = "Int32"
	Int64        = "Int64"
	Uint8        = "Uint8"
	Uint16       = "Uint16"
	Uint32       = "Uint32"
	Uint64       = "Uint64"
	Float        = "Float"
	Double       = "Double"
	DyNumber     = "DyNumber"
	String       = "String"
	Utf8         = "Utf8"
	JSON         = "Json"
	JSONDocument = "JsonDocument"
	Yson         = "Yson"
	UUID         = "Uuid"
	Date         = "Date"
	Datetime     = "Datetime"
	Timestamp    = "Timestamp"
	Interval     = "Interval"
	Date32       = "Date32"
	Datetime64   = "Datetime64"
	Timestamp64  = "Timestamp64"
	Interval64   = "Interval64"
	Serial       = "Serial"
	BigSerial    = "BigSerial"
	SmallSerial  = "SmallSerial"
)

// maxDecimalPrecision is the widest Decimal YDB takes.
const maxDecimalPrecision = 35

// fixedDecimal is the one Decimal YDB 25.1 takes without the parameterized
// decimal flag.
const fixedDecimal = "Decimal(22,9)"

// exactSpellings are YDB names that SQL also uses for another type. They are
// read as YDB's only when written exactly as YDB prints them.
var exactSpellings = map[string]string{
	"String":    String,
	"Int8":      Int8,
	"Float":     Float,
	"Date":      Date,
	"Datetime":  Datetime,
	"Timestamp": Timestamp,
	"Interval":  Interval,
}

// plainTypes are the declarations, upper-cased and with single spaces, that map
// to one YDB type whatever the target.
var plainTypes = map[string]string{
	"BOOL":    Bool,
	"BOOLEAN": Bool,

	"TINYINT":   Int8,
	"INT1":      Int8,
	"SMALLINT":  Int16,
	"INT2":      Int16,
	"INT16":     Int16,
	"INT":       Int32,
	"INTEGER":   Int32,
	"INT4":      Int32,
	"INT3":      Int32,
	"MEDIUMINT": Int32,
	"INT32":     Int32,
	"BIGINT":    Int64,
	"INT8":      Int64,
	"INT64":     Int64,

	"TINYINT UNSIGNED":   Uint8,
	"UINT8":              Uint8,
	"SMALLINT UNSIGNED":  Uint16,
	"UINT16":             Uint16,
	"INT UNSIGNED":       Uint32,
	"INTEGER UNSIGNED":   Uint32,
	"MEDIUMINT UNSIGNED": Uint32,
	"UINT32":             Uint32,
	"BIGINT UNSIGNED":    Uint64,
	"UINT64":             Uint64,

	"REAL":             Float,
	"FLOAT4":           Float,
	"FLOAT":            Double,
	"FLOAT8":           Double,
	"DOUBLE":           Double,
	"DOUBLE PRECISION": Double,

	"DYNUMBER": DyNumber,

	"TEXT":       Utf8,
	"TINYTEXT":   Utf8,
	"MEDIUMTEXT": Utf8,
	"LONGTEXT":   Utf8,
	"CLOB":       Utf8,
	"NCLOB":      Utf8,
	"NTEXT":      Utf8,
	"UTF8":       Utf8,
	// CockroachDB's STRING is text. YDB's String, written exactly so, is
	// bytes; see exactSpellings.
	"STRING": Utf8,

	"BYTES":      String,
	"BYTEA":      String,
	"BLOB":       String,
	"TINYBLOB":   String,
	"MEDIUMBLOB": String,
	"LONGBLOB":   String,

	"JSON":         JSON,
	"JSONB":        JSONDocument,
	"JSONDOCUMENT": JSONDocument,
	"YSON":         Yson,

	"UUID":             UUID,
	"UNIQUEIDENTIFIER": UUID,
}

// sizedStrings are the string types that take a length YDB does not keep.
var sizedStrings = map[string]string{
	"VARCHAR":                    Utf8,
	"CHARACTER VARYING":          Utf8,
	"NVARCHAR":                   Utf8,
	"NATIONAL CHARACTER VARYING": Utf8,
	"VARCHAR2":                   Utf8,
	"NVARCHAR2":                  Utf8,
	"CHAR":                       Utf8,
	"CHARACTER":                  Utf8,
	"NCHAR":                      Utf8,
	"BPCHAR":                     Utf8,
	"BINARY":                     String,
	"VARBINARY":                  String,
	"RAW":                        String,
}

// timestamps are the declarations of an instant. Each maps to Timestamp64
// where the target has the wide types and to Timestamp where it does not.
var timestamps = map[string]bool{
	"TIMESTAMP":                   true,
	"TIMESTAMPTZ":                 true,
	"TIMESTAMP WITH TIME ZONE":    true,
	"TIMESTAMP WITHOUT TIME ZONE": true,
	"DATETIME":                    true,
	"DATETIME2":                   true,
}

// wideNatives are the 64-bit date and time types, accepted in any case.
var wideNatives = map[string]string{
	"DATE32":      Date32,
	"DATETIME64":  Datetime64,
	"TIMESTAMP64": Timestamp64,
	"INTERVAL64":  Interval64,
}

// serials are the SERIAL spellings PostgreSQL and YDB share.
var serials = map[string]string{
	"SERIAL":      Serial,
	"SERIAL4":     Serial,
	"BIGSERIAL":   BigSerial,
	"SERIAL8":     BigSerial,
	"SMALLSERIAL": SmallSerial,
	"SERIAL2":     SmallSerial,
}

// noCounterpart are the declarations YDB has no type for on any line, with the
// reason a refusal gives.
var noCounterpart = map[string]string{
	"TIME":                   "YDB has no time-of-day type",
	"TIMETZ":                 "YDB has no time-of-day type",
	"TIME WITH TIME ZONE":    "YDB has no time-of-day type",
	"TIME WITHOUT TIME ZONE": "YDB has no time-of-day type",
	"TZDATE":                 "YDB refuses the Tz types in a table (`not supported by storage`)",
	"TZDATETIME":             "YDB refuses the Tz types in a table (`not supported by storage`)",
	"TZTIMESTAMP":            "YDB refuses the Tz types in a table (`not supported by storage`)",
	"TZDATE32":               "YDB refuses the Tz types in a table (`not supported by storage`)",
	"TZDATETIME64":           "YDB refuses the Tz types in a table (`not supported by storage`)",
	"TZTIMESTAMP64":          "YDB refuses the Tz types in a table (`not supported by storage`)",
	"XML":                    "YDB has no XML type",
	"MONEY":                  "YDB has no money type; declare a DECIMAL",
	"SMALLMONEY":             "YDB has no money type; declare a DECIMAL",
	"INET":                   "YDB has no network address type",
	"CIDR":                   "YDB has no network address type",
	"MACADDR":                "YDB has no network address type",
	"MACADDR8":               "YDB has no network address type",
	"POINT":                  "YDB has no geometric type",
	"LINE":                   "YDB has no geometric type",
	"LSEG":                   "YDB has no geometric type",
	"BOX":                    "YDB has no geometric type",
	"PATH":                   "YDB has no geometric type",
	"POLYGON":                "YDB has no geometric type",
	"CIRCLE":                 "YDB has no geometric type",
	"GEOMETRY":               "YDB has no geometric type",
	"GEOGRAPHY":              "YDB has no geometric type",
	"BIT":                    "YDB has no bit-string type",
	"VARBIT":                 "YDB has no bit-string type",
	"BIT VARYING":            "YDB has no bit-string type",
	"TSVECTOR":               "YDB has no text-search type",
	"TSQUERY":                "YDB has no text-search type",
	"HSTORE":                 "YDB has no key-value column type",
	"CITEXT":                 "YDB has no case-insensitive text; Utf8 compares bytes",
	"YEAR":                   "YDB has no year type; declare a SMALLINT",
	"ENUM":                   "YDB has no enum type",
	"SET":                    "YDB has no set type",
	"HALFVEC":                "YDB has no half-precision vector; a YDB vector index reads float, uint8, int8 or bit elements, so declare VECTOR(n)",
	"SPARSEVEC":              "YDB has no sparse vector; a YDB vector index reads every element of a dense vector, so declare VECTOR(n)",
	"INT4RANGE":              "YDB has no range type",
	"INT8RANGE":              "YDB has no range type",
	"NUMRANGE":               "YDB has no range type",
	"TSRANGE":                "YDB has no range type",
	"TSTZRANGE":              "YDB has no range type",
	"DATERANGE":              "YDB has no range type",
	"INT4MULTIRANGE":         "YDB has no range type",
	"INT8MULTIRANGE":         "YDB has no range type",
	"NUMMULTIRANGE":          "YDB has no range type",
	"TSMULTIRANGE":           "YDB has no range type",
	"TSTZMULTIRANGE":         "YDB has no range type",
	"DATEMULTIRANGE":         "YDB has no range type",
}

// Map returns the YDB type a declared column type lands on for a target with
// caps. The declaration is trimmed and read case-insensitively, except for the
// names exactSpellings lists. A type with no YDB counterpart, or one whose
// counterpart the target lacks, is a *[Refusal]; nothing is passed through
// unread, because a name YDB does not know fails at apply time on a server
// that has already run the statements before it.
func Map(declared string, caps Capabilities) (Mapping, error) {
	trimmed := strings.TrimSpace(declared)
	if trimmed == "" {
		return Mapping{}, &Refusal{Declared: "an empty type", Reason: "a column needs a type"}
	}
	if refusal := containerRefusal(trimmed); refusal != nil {
		return Mapping{}, refusal
	}
	if trimmed == "Varchar" || strings.HasPrefix(trimmed, "Varchar(") {
		return Mapping{}, &Refusal{Declared: trimmed, Reason: "YDB reads Varchar as String, a byte string; " +
			"declare Utf8 (or VARCHAR) for text and String for bytes"}
	}
	if native, ok := exactSpellings[trimmed]; ok {
		return Mapping{Type: native}, nil
	}

	base, arguments, ok := splitArguments(trimmed)
	if !ok {
		return Mapping{}, &Refusal{Declared: trimmed, Reason: "the argument list does not close"}
	}
	name := strings.Join(strings.Fields(strings.ToUpper(base)), " ")

	if reason, refused := noCounterpart[name]; refused {
		return Mapping{}, &Refusal{Declared: trimmed, Reason: reason}
	}
	if strings.HasPrefix(name, "PG") {
		return Mapping{}, &Refusal{Declared: trimmed, Reason: "the PostgreSQL types inside YDB are behind a " +
			"feature flag that is off by default (`EnableTablePgTypes`)"}
	}
	return mapNamed(trimmed, name, arguments, caps)
}

// mapNamed answers a declaration whose name is not refused outright.
func mapNamed(declared, name string, arguments []string, caps Capabilities) (Mapping, error) {
	if name == "FLOAT" && len(arguments) == 1 {
		return floatWithPrecision(declared, arguments[0])
	}
	if mapped, ok := plainTypes[name]; ok {
		if integers[mapped] && len(arguments) == 1 {
			return displayWidth(declared, mapped, arguments[0])
		}
		return withoutArguments(declared, mapped, arguments)
	}
	if mapped, ok := sizedStrings[name]; ok {
		return sizedString(declared, mapped, arguments)
	}
	if mapped, ok := serials[name]; ok {
		if !caps.Has(SerialColumns) {
			return Mapping{}, &Refusal{Declared: declared, Key: SerialColumns,
				Reason: "this target has no Serial column type"}
		}
		mapping, err := withoutArguments(declared, mapped, arguments)
		mapping.Serial = err == nil
		return mapping, err
	}
	if mapped, ok := wideNatives[name]; ok {
		if !caps.Has(WideDateTimeTypes) {
			return Mapping{}, &Refusal{Declared: declared, Key: WideDateTimeTypes,
				Reason: "the 64-bit date and time types are behind a flag that is off on this line"}
		}
		return withoutArguments(declared, mapped, arguments)
	}
	switch {
	case name == "VECTOR":
		return vector(declared, arguments)
	case name == "DECIMAL" || name == "NUMERIC" || name == "DEC":
		return decimal(declared, arguments, caps)
	case name == "DATE":
		return withoutArguments(declared, wideOrNarrow(caps, Date32, Date), arguments)
	case name == "INTERVAL":
		return withoutArguments(declared, wideOrNarrow(caps, Interval64, Interval), arguments)
	case timestamps[name]:
		return timestamp(declared, arguments, caps)
	}
	return Mapping{}, &Refusal{Declared: declared, Reason: "YDB has no type of that name"}
}

// wideOrNarrow picks the 64-bit type where the target has it.
func wideOrNarrow(caps Capabilities, wide, narrow string) string {
	if caps.Has(WideDateTimeTypes) {
		return wide
	}
	return narrow
}

// withoutArguments maps a type that takes no argument list, and refuses one
// that came with one rather than dropping it.
func withoutArguments(declared, mapped string, arguments []string) (Mapping, error) {
	if len(arguments) > 0 {
		return Mapping{}, &Refusal{Declared: declared, Reason: mapped + " takes no arguments"}
	}
	return Mapping{Type: mapped}, nil
}

// integers are the YDB integer types, which a MySQL declaration may give a
// display width.
var integers = map[string]bool{
	Int8: true, Int16: true, Int32: true, Int64: true,
	Uint8: true, Uint16: true, Uint32: true, Uint64: true,
}

// displayWidth maps MySQL's `INT(11)`. The number is a display width, not a
// range, so the integer type is the same and the width is reported as dropped.
func displayWidth(declared, mapped, argument string) (Mapping, error) {
	width, err := strconv.Atoi(argument)
	if err != nil || width < 1 {
		return Mapping{}, &Refusal{Declared: declared, Reason: "the display width is not a positive integer"}
	}
	return Mapping{Type: mapped, Dropped: "display width " + argument}, nil
}

// sizedString maps a string type whose length YDB does not keep. MAX and an
// absent length drop nothing.
func sizedString(declared, mapped string, arguments []string) (Mapping, error) {
	switch {
	case len(arguments) == 0:
		return Mapping{Type: mapped}, nil
	case len(arguments) > 1:
		return Mapping{}, &Refusal{Declared: declared, Reason: "a string type takes one length"}
	case strings.EqualFold(arguments[0], "MAX"):
		return Mapping{Type: mapped}, nil
	}
	length, err := strconv.Atoi(arguments[0])
	if err != nil || length < 1 {
		return Mapping{}, &Refusal{Declared: declared, Reason: "the length is not a positive integer"}
	}
	return Mapping{Type: mapped, Dropped: "length " + arguments[0]}, nil
}

// decimal maps DECIMAL(p, s). A missing scale is 0, as SQL defines it; a
// missing precision is refused, because YDB's own `Decimal` with no arguments
// is a parse error and any precision chosen here would be a guess.
func decimal(declared string, arguments []string, caps Capabilities) (Mapping, error) {
	if len(arguments) == 0 || len(arguments) > 2 {
		return Mapping{}, &Refusal{Declared: declared,
			Reason: "a YDB Decimal needs a precision and a scale; declare DECIMAL(p,s)"}
	}
	precision, err := strconv.Atoi(arguments[0])
	if err != nil || precision < 1 || precision > maxDecimalPrecision {
		return Mapping{}, &Refusal{Declared: declared,
			Reason: fmt.Sprintf("the precision must be between 1 and %d", maxDecimalPrecision)}
	}
	scale := 0
	if len(arguments) == 2 {
		scale, err = strconv.Atoi(arguments[1])
		if err != nil || scale < 0 || scale > precision {
			return Mapping{}, &Refusal{Declared: declared, Reason: "the scale must be between 0 and the precision"}
		}
	}
	mapped := fmt.Sprintf("Decimal(%d,%d)", precision, scale)
	if mapped != fixedDecimal && !caps.Has(ParameterizedDecimal) {
		return Mapping{}, &Refusal{Declared: declared, Key: ParameterizedDecimal,
			Reason: "this line has Decimal(22,9) only"}
	}
	return Mapping{Type: mapped}, nil
}

// MaxVectorDimension is the most elements a YDB vector index takes: 25.3.1.25
// through 26.2.1.14 answer `Invalid vector_dimension: 16385 should be between
// 1 and 16384`, and every line is held to that limit.
const MaxVectorDimension = 16384

// vector maps VECTOR(n) to the String column a YDB vector index reads: the
// vector is bytes there, `Knn::ToBinaryStringFloat` of its elements. The
// column keeps no dimension, so a declared one is reported as dropped and
// carried in [Mapping.Dimension]. A second argument, Oracle's element format,
// is refused: on YDB the element type is the index's vector_type.
func vector(declared string, arguments []string) (Mapping, error) {
	switch len(arguments) {
	case 0:
		return Mapping{Type: String}, nil
	case 1:
	default:
		return Mapping{}, &Refusal{Declared: declared, Reason: "a YDB vector column takes a dimension only; " +
			"the element type is the vector index's vector_type"}
	}
	dimension, err := strconv.ParseUint(arguments[0], 10, 64)
	if err != nil || dimension < 1 || dimension > MaxVectorDimension {
		return Mapping{}, &Refusal{Declared: declared,
			Reason: fmt.Sprintf("the dimension must be between 1 and %d, the most a YDB vector index takes", MaxVectorDimension)}
	}
	return Mapping{
		Type:      String,
		Dropped:   "dimension " + arguments[0] + " (YDB stores a vector as bytes; its vector index keeps the dimension)",
		Dimension: dimension,
	}, nil
}

// timestamp maps an instant. YDB keeps microseconds, so a declared precision of
// 6 drops nothing and any other precision is reported.
func timestamp(declared string, arguments []string, caps Capabilities) (Mapping, error) {
	mapped := wideOrNarrow(caps, Timestamp64, Timestamp)
	switch {
	case len(arguments) == 0:
		return Mapping{Type: mapped}, nil
	case len(arguments) > 1:
		return Mapping{}, &Refusal{Declared: declared, Reason: "a timestamp takes one precision"}
	}
	precision, err := strconv.Atoi(arguments[0])
	if err != nil || precision < 0 || precision > 9 {
		return Mapping{}, &Refusal{Declared: declared, Reason: "the precision is not between 0 and 9"}
	}
	if precision == 6 {
		return Mapping{Type: mapped}, nil
	}
	return Mapping{Type: mapped, Dropped: "precision " + arguments[0] + " (YDB keeps microseconds)"}, nil
}

// floatWithPrecision maps FLOAT(p) the way SQL does: up to 24 bits is single
// precision and up to 53 is double.
func floatWithPrecision(declared, argument string) (Mapping, error) {
	bits, err := strconv.Atoi(argument)
	switch {
	case err != nil || bits < 1 || bits > 53:
		return Mapping{}, &Refusal{Declared: declared, Reason: "the precision is not between 1 and 53"}
	case bits <= 24:
		return Mapping{Type: Float}, nil
	default:
		return Mapping{Type: Double}, nil
	}
}

// containerRefusal refuses the shapes YDB cannot store in a row table at all:
// arrays and YQL's container types.
func containerRefusal(declared string) *Refusal {
	upper := strings.ToUpper(declared)
	switch {
	case strings.HasSuffix(upper, "]") || strings.HasPrefix(upper, "ARRAY"):
		return &Refusal{Declared: declared, Reason: "YDB has no array column type"}
	case strings.HasSuffix(upper, "?"):
		return &Refusal{Declared: declared, Reason: "nullability is declared with NOT NULL, not with a `?` type"}
	}
	for _, container := range []string{"LIST<", "OPTIONAL<", "STRUCT<", "TUPLE<", "DICT<", "SET<", "VARIANT<", "TAGGED<", "STREAM<", "FLOW<"} {
		if strings.HasPrefix(upper, container) {
			return &Refusal{Declared: declared,
				Reason: "YDB stores only primitive types in a table column (`Only YQL data types and PG types are currently supported`)"}
		}
	}
	return nil
}

// splitArguments separates `NAME(a, b)` into its name and trimmed arguments.
// A declaration without a list has no arguments; one whose list does not close
// at the end is malformed.
func splitArguments(declared string) (base string, arguments []string, ok bool) {
	open := strings.Index(declared, "(")
	if open < 0 {
		return declared, nil, true
	}
	if !strings.HasSuffix(declared, ")") {
		return "", nil, false
	}
	inner := declared[open+1 : len(declared)-1]
	for argument := range strings.SplitSeq(inner, ",") {
		arguments = append(arguments, strings.TrimSpace(argument))
	}
	return strings.TrimSpace(declared[:open]), arguments, true
}
