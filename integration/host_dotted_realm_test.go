//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// dottedTables declares, in YQL, a table at the database root whose name has
// a dot in it, ptah_ydb_dotted.b, and the table b in the directory
// ptah_ydb_dotted, each with its own columns and an index of one name.
const dottedTables = "CREATE TABLE `ptah_ydb_dotted.b` (id Int64 NOT NULL, root_note Utf8, PRIMARY KEY (id), INDEX note_ix GLOBAL SYNC ON (root_note));\n" +
	"CREATE TABLE `ptah_ydb_dotted/b` (id Int64 NOT NULL, nested_note Utf8, PRIMARY KEY (id), INDEX note_ix GLOBAL SYNC ON (nested_note));\n"

// Ported from the plan half of the host's
// TestYDBDottedNames_ARootTableAndADirectoryTableStayTwo: the host plans both
// tables through this provider, applies them, and the next plan is empty. The
// realm's read holds them as two tables with their own columns and indexes.
// The run reads the root and the directory with no schema selector, so it is
// confined to a dev realm of its own: on the shared test database the root
// holds other runs' tables. The run's schema lock is a node at the realm's
// root, which the cleanup removes with the tables.
func TestHostKeepsARootTableAndADirectoryTableTwo_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, _, _ := lockFixture(c)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	address, root := realmFixture(c, opened, base)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, root)
	})

	first := hostPlannedAt(c, address, "", "sql", dottedTables)
	applied, applyErr := hostApplyEverywhereAs(c, address, "schema.sql", dottedTables)
	settled := hostPlannedAt(c, address, "", "sql", dottedTables)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	snapshot, inspectErr := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{"/", "ptah_ydb_dotted"}})

	c.Assert(first, qt.HasLen, 2, qt.Commentf("%v", first))
	c.Assert(applyErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(applied, qt.Matches, `(?s).*Schema apply completed successfully.*`)
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(inspectErr, qt.IsNil)
	atRoot, inDirectory := "|ptah_ydb_dotted.b", "ptah_ydb_dotted|b"
	c.Assert(tablesRead(c, snapshot, atRoot, inDirectory), qt.DeepEquals, map[string]readTable{
		atRoot:      {Normalized: atRoot, Columns: []string{"id", "root_note"}, Indexes: []string{"note_ix ON root_note"}},
		inDirectory: {Normalized: inDirectory, Columns: []string{"id", "nested_note"}, Indexes: []string{"note_ix ON nested_note"}},
	})
}
