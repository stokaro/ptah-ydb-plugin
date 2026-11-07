package tablecreate_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func uniqueTable() *protocol.DeclaredTable {
	return &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{
			{Name: "id", Type: "Int64", Primary: true, Unique: true},
			{Name: "tenant", Type: "Int64", Nullable: true},
			{Name: "login", Type: "Utf8", Nullable: true},
			{Name: "email", Type: "Utf8", Nullable: true, Unique: true},
		},
		Constraints: []*protocol.DeclaredConstraint{
			{Name: "uq_tenant_login", Type: "UNIQUE", Columns: []string{"tenant", "login"}, IncludeColumns: []string{"email"}},
			{Type: "UNIQUE", Columns: []string{"login"}},
			{Type: "UNIQUE", Columns: []string{"id"}},
		},
	}
}

// UNIQUE declarations become the same native indexes as explicit indexes,
// while a UNIQUE over the primary key needs no second index. The source is
// kept intact for the host's subsequent comparison.
func TestCompileUniqueDeclarations(t *testing.T) {
	c := testcheck.New(t)
	table := uniqueTable()
	before := proto.CloneOf(table)
	created, err := tablecreate.Compile(owner(), table, nil)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(table, before), qt.IsTrue)
	c.Assert(created.Indexes, qt.HasLen, 3)
	for i, want := range []struct {
		name, key, cover string
	}{
		{"uq_tenant_login", "tenant,login", "email"},
		{"records_login_key", "login", ""},
		{"records_email_key", "email", ""},
	} {
		index := created.Indexes[i]
		c.Assert(index.GetName(), qt.Equals, want.name)
		c.Assert(strings.Join(index.GetIndexColumns(), ","), qt.Equals, want.key)
		c.Assert(strings.Join(index.GetDataColumns(), ","), qt.Equals, want.cover)
		c.Assert(index.GetGlobalUniqueIndex(), qt.IsNotNil)
	}
}

func TestCompileUniqueFoldsPrimaryMarksWithoutAnExplicitKey(t *testing.T) {
	c := testcheck.New(t)
	table := uniqueTable()
	table.PrimaryKey = nil
	created, err := tablecreate.Compile(owner(), table, nil)
	c.Assert(err, qt.IsNil)
	c.Assert(created.PrimaryKey, qt.DeepEquals, []string{"id"})
	c.Assert(created.Indexes, qt.HasLen, 3)
}

func TestCompileUniqueKeepsItsCommentForTheHost(t *testing.T) {
	c := testcheck.New(t)
	table := uniqueTable()
	table.Constraints[0].Comment = "One login each"
	before := proto.CloneOf(table)
	created, err := tablecreate.Compile(owner(), table, nil)
	c.Assert(err, qt.IsNil)
	c.Assert(created.Indexes[0].GetName(), qt.Equals, "uq_tenant_login")
	c.Assert(proto.Equal(table, before), qt.IsTrue)
}

func TestCompileRefusesUnsupportedUniqueDeclarations(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*protocol.DeclaredTable)
		want   string
	}{
		{"deferrable", func(d *protocol.DeclaredTable) { d.Constraints[0].Deferrable = true }, `.*deferrable.*`},
		{"not enforced", func(d *protocol.DeclaredTable) { d.Constraints[0].NotEnforced = true }, `.*NOT ENFORCED.*`},
		{"nulls not distinct", func(d *protocol.DeclaredTable) { d.Constraints[0].NullsDistinct = new(false) }, `.*NULLs as equal.*`},
		{"other table", func(d *protocol.DeclaredTable) { d.Constraints[0].Table = "other" }, `.*names another table.*`},
		{"missing column", func(d *protocol.DeclaredTable) { d.Constraints[0].Columns = []string{"missing"} }, `.*does not declare.*`},
		{"duplicate name", func(d *protocol.DeclaredTable) { d.Constraints[1].Name = "uq_tenant_login" }, `.*two of its indexes are named.*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			table := uniqueTable()
			test.change(table)
			created, err := tablecreate.Compile(owner(), table, nil)
			c.Assert(created, qt.IsNil)
			c.Assert(err, qt.ErrorMatches, test.want)
		})
	}
}
