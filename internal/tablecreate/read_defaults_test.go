package tablecreate_test

import (
	"math"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// readTable is table with the column settings of column, as a declaration the
// host converts from a read carries them beside the common columns.
func readTable(c *qt.C, table *protocol.DeclaredTable, column, settings string) *protocol.DeclaredTable {
	c.Helper()
	table.Features = &protocol.CapturedFeatures{Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED,
		Records: []*protocol.ComparisonRecord{facet(c, schema.ColumnProperties, column, settings)}}}
	return table
}

// titledTable declares app.records with key id and a NOT NULL title whose
// default is untitled.
func titledTable() *protocol.DeclaredTable {
	return &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Int64", Primary: true}, {Name: "title", Type: "Utf8", DefaultSet: true, DefaultValue: "untitled"}}}
}

// A read states a Serial twice: as the common column's auto_increment, start
// and increment, and as the native sequence its default fills from. A
// declaration converted from it compiles to the sequence the common column
// gives, whichever spelling of the same sequence the native one is.
func TestCompileReadSerialColumn_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		id       *protocol.DeclaredColumn
		settings string
		sequence string
	}{
		{name: "the sequence a read writes out", id: &protocol.DeclaredColumn{Type: "Int64", AutoIncrement: true},
			settings: `{"sequence":{"name":"_serial_column_id","minValue":"1","maxValue":"9223372036854775807","startValue":"1","cache":"1","increment":"1","cycle":false,"dataType":{"typeId":"INT64"}}}`,
			sequence: sequenceJSON(math.MaxInt64, "1", "1")},
		{name: "a start and an increment", id: &protocol.DeclaredColumn{Type: "Int64", AutoIncrement: true, IdentityStart: "100", IdentityIncrement: "5"},
			settings: `{"sequence":{"startValue":"100","increment":"5"}}`, sequence: sequenceJSON(math.MaxInt64, "100", "5")},
		{name: "a 32-bit Serial whose sequence ends at the Int64 maximum", id: &protocol.DeclaredColumn{Type: "Int32", AutoIncrement: true},
			settings: `{"sequence":{"maxValue":"9223372036854775807"}}`, sequence: sequenceJSON(math.MaxInt32, "1", "1")},
		{name: "a sequence that states nothing", id: &protocol.DeclaredColumn{Type: "BIGSERIAL"}, settings: `{"sequence":{}}`,
			sequence: sequenceJSON(math.MaxInt64, "1", "1")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := tablecreate.Compile(owner(), readTable(c, serialTable(test.id), "id", test.settings), serialCapabilities())
			c.Assert(err, qt.IsNil)
			sequence, err := protojson.Marshal(request.Columns[0].GetFromSequence())
			c.Assert(err, qt.IsNil)
			c.Assert(string(sequence), qt.JSONEquals, jsonValue(c, test.sequence))
		})
	}
}

// A native sequence that is not the one the common column gives is refused:
// it is a second declaration of the column's default, and the two disagree.
func TestCompileReadSerialColumn_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name     string
		id       *protocol.DeclaredColumn
		settings string
		want     string
	}{
		{name: "a column that is no Serial", id: &protocol.DeclaredColumn{Type: "Int64"}, settings: `{"sequence":{}}`,
			want: `column "id" of table "app/records" takes its default from a sequence, and only a Serial column has one`},
		{name: "another start", id: &protocol.DeclaredColumn{Type: "Int64", AutoIncrement: true}, settings: `{"sequence":{"startValue":"10"}}`,
			want: `column "id" of table "app/records": its native sequence starts at 10 and steps by 1, and the column declares a start of 1 and an increment of 1`},
		{name: "another increment", id: &protocol.DeclaredColumn{Type: "Int64", AutoIncrement: true, IdentityIncrement: "5"}, settings: `{"sequence":{}}`,
			want: `column "id" of table "app/records": its native sequence starts at 1 and steps by 1, and the column declares a start of 1 and an increment of 5`},
		{name: "a cache", id: &protocol.DeclaredColumn{Type: "Int64", AutoIncrement: true}, settings: `{"sequence":{"cache":"5"}}`,
			want: `column "id" of table "app/records": sequence requires minimum 1, cache 1, and no cycling`},
		{name: "another name", id: &protocol.DeclaredColumn{Type: "Int64", AutoIncrement: true}, settings: `{"sequence":{"name":"other"}}`,
			want: `column "id" of table "app/records": sequence name differs from the column's owned sequence`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := tablecreate.Compile(owner(), readTable(c, serialTable(test.id), "id", test.settings), serialCapabilities())
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(tablecreate.IsUnsupported(err), qt.IsTrue)
			c.Assert(request, qt.IsNil)
		})
	}
}

// A read states a literal default twice too, as the common column's default
// and as the native literal, and a declaration converted from it compiles to
// the one default they agree on.
func TestCompileReadLiteralDefault_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request, err := tablecreate.Compile(owner(), readTable(c, titledTable(), "title", `{"literal":{"type":{"typeId":"UTF8"},"value":{"textValue":"untitled"}}}`), nil)
	c.Assert(err, qt.IsNil)
	literal, err := protojson.Marshal(request.Columns[1].GetFromLiteral())
	c.Assert(err, qt.IsNil)
	c.Assert(string(literal), qt.JSONEquals, jsonValue(c, `{"type":{"typeId":"UTF8"},"value":{"textValue":"untitled"}}`))
}

// A native literal other than the common default is a second default, and is
// refused.
func TestCompileReadLiteralDefault_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request, err := tablecreate.Compile(owner(), readTable(c, titledTable(), "title", `{"literal":{"type":{"typeId":"UTF8"},"value":{"textValue":"other"}}}`), nil)
	c.Assert(err, qt.ErrorMatches, `a column default must have one declaration, common or native`)
	c.Assert(tablecreate.IsUnsupported(err), qt.IsTrue)
	c.Assert(request, qt.IsNil)
}
