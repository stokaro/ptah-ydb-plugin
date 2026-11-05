//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The host's TestYDBUniqueConstraint_AddedToATableThatExists_FailurePath,
// through a Go source. A unique index added to items, which exists, on a
// session without unique_index_on_existing_table is refused when the host
// plans it, in the words of the host's embedded engine, and the table keeps
// the indexes it held.
func TestHostRefusesAUniqueIndexOnATableThatExists_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	requireSessionCapability(c, session, tablecreate.UniqueIndexOnExistingTableCapability, false)

	output, err := hostApply(c, directory, hostGlobalItems(directory, "idx_items_kind", ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	withUnique := hostGlobalItems(directory, "idx_items_kind", "\t//ptah:schema:index name=\"uq_items_price\" fields=\"price\" unique=\"true\"\n")
	output, err = hostApply(c, directory, withUnique)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Matches, `(?s).*adding unique index "uq_items_price" to table "[^"]+\.items", which exists already, `+
		`which requires target capability unique_index_on_existing_table, unavailable on this ydb target; YDB keeps it behind feature flag EnableAddUniqueIndex: turn the flag on.*`)
	_, added := heldIndexColumns(c, opened, absolute)["uq_items_price"]
	c.Assert(added, qt.IsFalse)
}

// A 26.2 server started with enable_add_unique_index adds a unique index to a
// table that exists, and once EnableAddUniqueIndex is turned off a new
// session no longer establishes it: the host's plan is refused as on 25.1.
func TestHostRefusesAUniqueIndexWithTheFlagOff_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	requireSessionCapability(c, session, tablecreate.UniqueIndexOnExistingTableCapability, true)
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableAddUniqueIndex": false})

	output, err := hostApply(c, directory, hostGlobalItems(directory, "idx_items_kind", ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	withUnique := hostGlobalItems(directory, "idx_items_kind", "\t//ptah:schema:index name=\"uq_items_price\" fields=\"price\" unique=\"true\"\n")
	output, err = hostApply(c, directory, withUnique)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Matches, `(?s).*adding unique index "uq_items_price" to table "[^"]+\.items", which exists already, `+
		`which requires target capability unique_index_on_existing_table, unavailable on this ydb target; YDB keeps it behind feature flag EnableAddUniqueIndex: turn the flag on.*`)
	_, added := heldIndexColumns(c, opened, absolute)["uq_items_price"]
	c.Assert(added, qt.IsFalse)
}

// withoutUniqueIndexOnExistingTable leaves the test without
// unique_index_on_existing_table: on a line whose session establishes it, as
// a 26.2 server started with enable_add_unique_index does, EnableAddUniqueIndex
// is turned off until the test ends, and a new session no longer reports it.
func withoutUniqueIndexOnExistingTable(c *qt.C, opened *connection.Connection) {
	c.Helper()
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session := func() *protocol.Session {
		session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
		c.Assert(err, qt.IsNil)
		c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
		return session
	}
	if session().Capabilities[tablecreate.UniqueIndexOnExistingTableCapability] {
		newClusterFlags(c, opened).set(c, map[string]bool{"EnableAddUniqueIndex": false})
	}
	c.Assert(session().Capabilities[tablecreate.UniqueIndexOnExistingTableCapability], qt.IsFalse)
}

// Ported from the host's
// TestYDBUniqueConstraint_AddedToATableThatExists_FailurePath. A UNIQUE
// constraint added to accounts, which exists, is the unique index YDB keeps
// for it, so on a session without unique_index_on_existing_table the host's
// plan is refused before anything runs, in the words of the host's embedded
// engine, and naming the constraint. The table keeps the one unique index it
// was created with. TestHostRefusesAUniqueIndexOnATableThatExists_FailurePath
// holds the same refusal for an index directive.
func TestHostRefusesAUniqueConstraintOnATableThatExists_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := accountsFixture(c)
	withoutUniqueIndexOnExistingTable(c, opened)
	output, err := hostApply(c, directory, withoutLines(uniqueAccounts(directory), "ptah:schema:constraint"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	output, err = hostApply(c, directory, uniqueAccounts(directory))

	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Matches, `(?s).*adding unique index "uq_accounts_tenant_login" to table "[^"]+\.accounts", which exists already, `+
		`which requires target capability unique_index_on_existing_table, unavailable on this ydb target; YDB keeps it behind feature flag EnableAddUniqueIndex: turn the flag on.*`)
	c.Assert(heldIndexColumns(c, opened, absolute), qt.DeepEquals, map[string][2][]string{"accounts_email_key": {{"email"}, nil}})
}
