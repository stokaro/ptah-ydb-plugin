//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
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
		`which requires target capability unique_index_on_existing_table, unavailable on this ydb target.*`)
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
		`which requires target capability unique_index_on_existing_table, unavailable on this ydb target.*`)
	_, added := heldIndexColumns(c, opened, absolute)["uq_items_price"]
	c.Assert(added, qt.IsFalse)
}
