package tablecreate_test

import (
	"math"
	"regexp"
	"strconv"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// serialCapabilities are those of every YDB line the provider supports: Serial
// columns and sequence settings, and no line that keeps a narrow Serial's
// range through an ALTER SEQUENCE.
func serialCapabilities() map[string]bool {
	return map[string]bool{"serial_columns": true, "serial_sequence_options": true, "serial_sequence_keeps_range": false}
}

// serialTable declares app.records with key id as the column given and a
// nullable note.
func serialTable(id *protocol.DeclaredColumn) *protocol.DeclaredTable {
	id.Name, id.Primary = "id", true
	return &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{id, {Name: "note", Type: "Utf8", Nullable: true}}}
}

// sequenceJSON is the sequence compilation writes for a Serial whose type ends
// at maximum, with the start and the increment given.
func sequenceJSON(maximum int64, start, increment string) string {
	return `{"name":"_serial_column_id","minValue":"1","maxValue":"` + strconv.FormatInt(maximum, 10) + `","startValue":"` + start +
		`","cache":"1","increment":"` + increment + `","cycle":false}`
}

// A Serial is created as the integer its sequence fills, NOT NULL, with the
// sequence the native call creates it with: the column's own name, the
// maximum of its type, and the start and the increment the declaration
// states. Every spelling the host's embedded engine reads as a Serial lands on
// one.
func TestCompileSerialColumn_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		id       *protocol.DeclaredColumn
		storage  Ydb.Type_PrimitiveTypeId
		sequence string
	}{
		{name: "BIGSERIAL", id: &protocol.DeclaredColumn{Type: "BIGSERIAL"}, storage: Ydb.Type_INT64, sequence: sequenceJSON(math.MaxInt64, "1", "1")},
		{name: "BigSerial with a start and an increment", id: &protocol.DeclaredColumn{Type: "BigSerial", IdentityStart: "100", IdentityIncrement: "5"},
			storage: Ydb.Type_INT64, sequence: sequenceJSON(math.MaxInt64, "100", "5")},
		{name: "BIGINT with auto_increment", id: &protocol.DeclaredColumn{Type: "BIGINT", AutoIncrement: true, IdentityIncrement: "3"},
			storage: Ydb.Type_INT64, sequence: sequenceJSON(math.MaxInt64, "1", "3")},
		{name: "an Int64 identity", id: &protocol.DeclaredColumn{Type: "Int64", IdentityGeneration: "BY_DEFAULT", IdentityStart: "0100"},
			storage: Ydb.Type_INT64, sequence: sequenceJSON(math.MaxInt64, "100", "1")},
		{name: "SERIAL", id: &protocol.DeclaredColumn{Type: "SERIAL"}, storage: Ydb.Type_INT32, sequence: sequenceJSON(math.MaxInt32, "1", "1")},
		{name: "INT with auto_increment", id: &protocol.DeclaredColumn{Type: "INT", AutoIncrement: true}, storage: Ydb.Type_INT32,
			sequence: sequenceJSON(math.MaxInt32, "1", "1")},
		{name: "SMALLSERIAL", id: &protocol.DeclaredColumn{Type: "SMALLSERIAL"}, storage: Ydb.Type_INT16, sequence: sequenceJSON(math.MaxInt16, "1", "1")},
		{name: "a Serial declared nullable", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", Nullable: true}, storage: Ydb.Type_INT64,
			sequence: sequenceJSON(math.MaxInt64, "1", "1")},
		{name: "the defaults written out", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "1", IdentityIncrement: "1"}, storage: Ydb.Type_INT64,
			sequence: sequenceJSON(math.MaxInt64, "1", "1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := tablecreate.Compile(owner(), serialTable(test.id), serialCapabilities())
			c.Assert(err, qt.IsNil)
			id := request.Columns[0]
			c.Assert(id.Type.GetTypeId(), qt.Equals, test.storage)
			c.Assert(id.GetNotNull(), qt.IsTrue)
			sequence, err := protojson.Marshal(id.GetFromSequence())
			c.Assert(err, qt.IsNil)
			c.Assert(string(sequence), qt.JSONEquals, jsonValue(c, test.sequence))
			c.Assert(request.Columns[1].GetFromSequence(), qt.IsNil)
			_, err = tablecreate.CaptureRequest("create", owner(), request)
			c.Assert(err, qt.IsNil)
		})
	}
}

// What YDB has no Serial for, and a sequence declaration it cannot honor, is
// refused in the host's embedded engine's words.
func TestCompileSerialColumn_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		id   *protocol.DeclaredColumn
		caps map[string]bool
		want string
	}{
		{name: "an auto-increment Uint64", id: &protocol.DeclaredColumn{Type: "Uint64", AutoIncrement: true}, caps: serialCapabilities(),
			want: `column "id" of table "app/records": an auto-increment column is a Serial, which YDB has for Int16, Int32 and Int64 only, and the column is Uint64`},
		{name: "an auto-increment column without Serial columns", id: &protocol.DeclaredColumn{Type: "BIGINT", AutoIncrement: true},
			want: `column "id" of table "app/records" increments itself, which requires target capability serial_columns, unavailable on this ydb target`},
		{name: "a Serial type without Serial columns", id: &protocol.DeclaredColumn{Type: "BIGSERIAL"},
			want: `column "id" of table "app/records": BIGSERIAL requires target capability serial_columns: this target has no Serial column type`},
		{name: "GENERATED ALWAYS", id: &protocol.DeclaredColumn{Type: "BIGINT", IdentityGeneration: "ALWAYS"}, caps: serialCapabilities(),
			want: `column "id" of table "app/records": GENERATED ALWAYS refuses an explicit value, and a YDB Serial takes one`},
		{name: "identity options", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityOptions: "MINVALUE 5"}, caps: serialCapabilities(),
			want: `column "id" of table "app/records": a YDB Serial's sequence takes a start and an increment and nothing else, so declare those with identity_start and identity_increment rather than the options MINVALUE 5`},
		{name: "a default", id: &protocol.DeclaredColumn{Type: "BIGINT", AutoIncrement: true, DefaultSet: true, DefaultValue: "3"}, caps: serialCapabilities(),
			want: `column "id" of table "app/records": a Serial column takes its default from its sequence`},
		{name: "a start of 0", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "0"}, caps: serialCapabilities(),
			want: `column "id" of table "app/records": identity_start 0: a YDB Serial's sequence takes a start of 1 or more \(measured: ` + "`Start value: 0 cannot be less than min value: 1`" + `\)`},
		{name: "a negative increment", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityIncrement: "-1"}, caps: serialCapabilities(),
			want: `column "id" of table "app/records": identity_increment -1: a YDB Serial's sequence takes an increment of 1 or more .*`},
		{name: "a start that is no number", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "ten"}, caps: serialCapabilities(),
			want: `column "id" of table "app/records": identity_start "ten" is not a whole number YDB's ALTER SEQUENCE takes`},
		{name: "a start on a column that is not a Serial", id: &protocol.DeclaredColumn{Type: "Uint64", IdentityStart: "5"}, caps: serialCapabilities(),
			want: `column "id" of table "app/records" gives a sequence a start or an increment, and only a Serial column has a sequence`},
		{name: "settings without sequence options", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "5"},
			caps: map[string]bool{"serial_columns": true},
			want: `giving the sequence of column "id" of table "app/records" a start or an increment, which requires target capability serial_sequence_options, unavailable on this ydb target`},
		{name: "a start on a 32-bit Serial", id: &protocol.DeclaredColumn{Type: "SERIAL", IdentityStart: "100"}, caps: serialCapabilities(),
			want: `giving the sequence of column "id" of table "app/records" a start or an increment \(an ALTER SEQUENCE raises the maximum of a Serial's sequence from 2147483647 ` +
				`to the Int64 maximum, .*\), which requires target capability serial_sequence_keeps_range, unavailable on this ydb target`},
		{name: "an increment on a 16-bit Serial", id: &protocol.DeclaredColumn{Type: "SMALLINT", AutoIncrement: true, IdentityIncrement: "2"}, caps: serialCapabilities(),
			want: `giving the sequence of column "id" of table "app/records" a start or an increment \(an ALTER SEQUENCE raises the maximum of a SmallSerial's sequence from 32767 .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := tablecreate.Compile(owner(), serialTable(test.id), test.caps)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(tablecreate.IsUnsupported(err), qt.IsTrue)
			c.Assert(request, qt.IsNil)
		})
	}
}

// A column table has no Serial column.
func TestCompileSerialColumnTable_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	table := columnDeclaration(c, `{}`)
	table.Columns[1].Type, table.Columns[1].AutoIncrement = "BIGINT", true
	request, err := tablecreate.CompileColumnTable(owner(), table, serialCapabilities())
	c.Assert(err, qt.ErrorMatches, `column tables do not support defaults or Serial columns`)
	c.Assert(request, qt.IsNil)
}

// A creation request is held to the sequence compilation writes, so a forged
// operation cannot create one the declaration could not state.
func TestCreationRequestSerial_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name     string
		column   string
		sequence string
	}{
		{name: "another name", column: `{"typeId":"INT64"}`, sequence: `{"name":"other","minValue":"1","maxValue":"9223372036854775807","startValue":"1","cache":"1","increment":"1","cycle":false}`},
		{name: "a negative increment", column: `{"typeId":"INT64"}`, sequence: `{"name":"_serial_column_id","minValue":"1","maxValue":"9223372036854775807","startValue":"1","cache":"1","increment":"-1","cycle":false}`},
		{name: "a 32-bit Serial ending at the Int64 maximum", column: `{"typeId":"INT32"}`, sequence: sequenceJSON(math.MaxInt64, "1", "1")},
		{name: "a start past the maximum", column: `{"typeId":"INT16"}`, sequence: sequenceJSON(math.MaxInt16, "40000", "1")},
		{name: "a cycle", column: `{"typeId":"INT64"}`, sequence: `{"name":"_serial_column_id","minValue":"1","maxValue":"9223372036854775807","startValue":"1","cache":"1","increment":"1","cycle":true}`},
		{name: "a cache", column: `{"typeId":"INT64"}`, sequence: `{"name":"_serial_column_id","minValue":"1","maxValue":"9223372036854775807","startValue":"1","cache":"10","increment":"1","cycle":false}`},
		{name: "no start", column: `{"typeId":"INT64"}`, sequence: `{"name":"_serial_column_id","minValue":"1","maxValue":"9223372036854775807","cache":"1","increment":"1","cycle":false}`},
		{name: "a restart", column: `{"typeId":"INT64"}`, sequence: `{"name":"_serial_column_id","minValue":"1","maxValue":"9223372036854775807","startValue":"1","cache":"1","increment":"1","cycle":false,"setVal":{"nextValue":"5"}}`},
		{name: "a text column", column: `{"typeId":"UTF8"}`, sequence: sequenceJSON(math.MaxInt64, "1", "1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := creationRequest(c, `{"columns":[{"name":"id","type":`+test.column+`,"notNull":true,"fromSequence":`+test.sequence+`}],"primaryKey":["id"],"storeType":"STORE_TYPE_ROW"}`)
			operation, err := tablecreate.CaptureRequest("create", owner(), request)
			c.Assert(err, qt.IsNotNil)
			c.Assert(operation, qt.IsNil)
		})
	}
}

// A nullable Serial is refused: YDB creates one through the native call, and a
// Serial is NOT NULL.
func TestCreationRequestNullableSerial_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := creationRequest(c, `{"columns":[{"name":"id","type":{"optionalType":{"item":{"typeId":"INT64"}}},"notNull":false,"fromSequence":`+
		sequenceJSON(math.MaxInt64, "1", "1")+`}],"primaryKey":["id"],"storeType":"STORE_TYPE_ROW"}`)
	operation, err := tablecreate.CaptureRequest("create", owner(), request)
	c.Assert(err, qt.ErrorMatches, `a YDB Serial column is a NOT NULL Int16, Int32 or Int64`)
	c.Assert(operation, qt.IsNil)
}

// The TEXT form writes a Serial as its Serial type, and gives its sequence the
// start and the increment the request states with the ALTER SEQUENCE the
// host's embedded engine writes after the CREATE TABLE: RESTART WITH the start
// where it is not 1, since START alone leaves the next value at 1.
func TestRowTableStatementSerial_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name string
		id   *protocol.DeclaredColumn
		want string
	}{
		{name: "a BigSerial", id: &protocol.DeclaredColumn{Type: "BIGSERIAL"},
			want: "CREATE TABLE `app/records` (\n    `id` BigSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);"},
		{name: "a 32-bit Serial", id: &protocol.DeclaredColumn{Type: "INT", AutoIncrement: true},
			want: "CREATE TABLE `app/records` (\n    `id` Serial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);"},
		{name: "a 16-bit Serial", id: &protocol.DeclaredColumn{Type: "SMALLSERIAL"},
			want: "CREATE TABLE `app/records` (\n    `id` SmallSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);"},
		{name: "a start and an increment", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "5"},
			want: "CREATE TABLE `app/records` (\n    `id` BigSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);\n" +
				"ALTER SEQUENCE `/local/app/records/_serial_column_id` START WITH 100 INCREMENT BY 5 RESTART WITH 100;"},
		{name: "an increment alone", id: &protocol.DeclaredColumn{Type: "BIGINT", AutoIncrement: true, IdentityIncrement: "3"},
			want: "CREATE TABLE `app/records` (\n    `id` BigSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);\n" +
				"ALTER SEQUENCE `/local/app/records/_serial_column_id` START WITH 1 INCREMENT BY 3;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := tablecreate.Compile(owner(), serialTable(test.id), serialCapabilities())
			c.Assert(err, qt.IsNil)
			got, err := tablecreate.RowTableStatementIn("/local", "app/records", request)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

// A creation in no named database writes a Serial without the ALTER SEQUENCE
// that would give its sequence a start and an increment, since only the
// sequence's absolute path names it, and the two settings are what the text
// goes without; a Serial with YDB's own settings goes without nothing, and a
// creation in a database goes without nothing either.
func TestSequenceOmissions_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, database string
		id             *protocol.DeclaredColumn
		want           []tablecreate.Omission
	}{
		{name: "a start and an increment", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "5"},
			want: []tablecreate.Omission{{Column: "id", Property: "identity start", Detail: "100"}, {Column: "id", Property: "identity increment", Detail: "5"}}},
		{name: "an increment alone", id: &protocol.DeclaredColumn{Type: "BIGINT", AutoIncrement: true, IdentityIncrement: "3"},
			want: []tablecreate.Omission{{Column: "id", Property: "identity increment", Detail: "3"}}},
		{name: "YDB's own settings", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "1", IdentityIncrement: "1"}},
		{name: "a database", database: "/local", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := tablecreate.Compile(owner(), serialTable(test.id), serialCapabilities())
			c.Assert(err, qt.IsNil)

			omissions := tablecreate.SequenceOmissions(test.database, request)

			c.Assert(omissions, qt.DeepEquals, test.want)
		})
	}
}

// The text of a creation in no named database is its CREATE TABLE alone,
// whatever the Serial's sequence states.
func TestRowTableStatementSerialWithoutADatabase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request, err := tablecreate.Compile(owner(), serialTable(&protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "7", IdentityIncrement: "2"}), serialCapabilities())
	c.Assert(err, qt.IsNil)

	got, err := tablecreate.RowTableStatementIn("", "app/records", request)

	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.Equals, "CREATE TABLE `app/records` (\n    `id` BigSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);")
}

// A sequence the ALTER SEQUENCE cannot name, or would break, has no TEXT form.
func TestRowTableStatementSerial_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, database, sequence, column, want string
	}{
		{name: "a 32-bit Serial with a start", database: "/local", sequence: sequenceJSON(math.MaxInt32, "100", "1"), column: `{"typeId":"INT32"}`,
			want: `the sequence of column "id" gets a start or an increment, and the ALTER SEQUENCE that would give it one raises the maximum of a 16-bit or 32-bit Serial to the Int64 maximum`},
		{name: "a sequence on a column YQL has no Serial for", database: "/local", sequence: sequenceJSON(math.MaxInt64, "1", "1"), column: `{"typeId":"UINT64"}`,
			want: `column "id" takes its default from a sequence, and only a Serial column of an integer type has one in YQL`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := creationRequest(c, `{"columns":[{"name":"id","type":`+test.column+`,"notNull":true,"fromSequence":`+test.sequence+`}],"primaryKey":["id"]}`)
			got, err := tablecreate.RowTableStatementIn(test.database, "app/records", request)
			c.Assert(err, qt.ErrorIs, tablecreate.ErrNoTextForm)
			c.Assert(err, qt.ErrorMatches, ".*"+regexp.QuoteMeta(test.want))
			c.Assert(got, qt.Equals, "")
		})
	}
}

// A projection predicts what a Serial is created as, settings the target does
// not take included: that refusal belongs to validation and planning, which
// report it in the host's words, where a refused projection reaches the host
// as an error without them.
func TestProjectSerialCreation_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := projectionRequest(c)
	request.TargetFeatures = []string{provider.ColumnsFeature}
	request.Capabilities = serialCapabilities()
	declared := request.Tables[0].Declaration
	serial := serialTable(&protocol.DeclaredColumn{Type: "SERIAL", IdentityStart: "100"})
	declared.PrimaryKey, declared.Columns, declared.Features.Facets.Records = serial.PrimaryKey, serial.Columns, nil
	result, err := backend.New().ProjectTableCreations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	id := result.Tables[0].Columns[0]
	c.Assert([]any{id.ColumnType, id.IsNullable, id.AutoIncrement, id.IdentityStart, id.IdentityIncrement}, qt.DeepEquals, []any{"Int32", "NO", true, "100", ""})
}

// serialSettingsTargets are Serial settings a target does not take, with the
// refusal a creation of the table gives.
var serialSettingsTargets = []struct {
	name string
	id   *protocol.DeclaredColumn
	caps map[string]bool
	want string
}{
	{name: "a start on a 32-bit Serial", id: &protocol.DeclaredColumn{Type: "SERIAL", IdentityStart: "100"}, caps: serialCapabilities(),
		want: `giving the sequence of column "id" of table "app/records" a start or an increment \(.*\), which requires target capability serial_sequence_keeps_range, unavailable on this ydb target`},
	{name: "settings without sequence options", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityIncrement: "2"}, caps: map[string]bool{"serial_columns": true},
		want: `giving the sequence of column "id" of table "app/records" a start or an increment, which requires target capability serial_sequence_options, unavailable on this ydb target`},
}

// Validation takes Serial settings a target does not take, as the host's
// embedded engine validates them: a table that already holds them changes
// nothing, and validation cannot tell. G113 of stokaro/ptah#4204.
func TestValidateSerialSettings_HappyPath(t *testing.T) {
	for _, test := range serialSettingsTargets {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(tablecreate.ValidateDeclaration(tableSubject(), serialTable(test.id), test.caps), qt.IsNil)
		})
	}
}

// A creation refuses the settings a target does not take, before anything is
// written.
func TestCompileSerialSettings_FailurePath(t *testing.T) {
	for _, test := range serialSettingsTargets {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			created, err := tablecreate.Compile(owner(), serialTable(test.id), test.caps)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(tablecreate.IsUnsupported(err), qt.IsTrue)
			c.Assert(created, qt.IsNil)
		})
	}
}
