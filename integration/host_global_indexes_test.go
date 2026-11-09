//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// hostGlobalItems declares items in directory as the host's global-index
// tests do: a synchronous index on kind, a unique index on sku, and the
// further index directives given, each a line of the price field.
func hostGlobalItems(directory, kindIndex, priceIndexes string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"sku\" type=\"TEXT\"\n\t//ptah:schema:index name=\"uq_items_sku\" fields=\"sku\" unique=\"true\"\n\tSKU string\n" +
		"\t//ptah:schema:field name=\"kind\" type=\"TEXT\"\n\t//ptah:schema:index name=\"" + kindIndex + "\" fields=\"kind\"\n\tKind string\n" +
		"\t//ptah:schema:field name=\"price\" type=\"BIGINT\"\n" + priceIndexes + "\tPrice int64\n}\n"
}

// itemsFixture is the lock fixture with items dropped when the test ends, and
// the absolute path of items.
func itemsFixture(c *qt.C) (opened *connection.Connection, directory, absolute string) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	absolute = path.Join(database, directory, "items")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	return opened, directory, absolute
}

// heldIndexColumns reads the key and covered columns of each index a table
// holds back from the table service, by name.
func heldIndexColumns(c *qt.C, opened *connection.Connection, absolute string) map[string][2][]string {
	c.Helper()
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(session.Close(), qt.IsNil) }()
	described, err := session.Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	result := make(map[string][2][]string)
	for _, index := range described.GetIndexes() {
		result[index.GetName()] = [2][]string{index.GetIndexColumns(), index.GetDataColumns()}
	}
	return result
}

// The host adds an asynchronous covering index to a table that exists and
// holds rows, as the host's TestYDBGlobalIndexes_AddedToATableThatExists
// does: the server builds it over the rows, the table reads back with it, and
// planning again finds nothing to change. The host also gives the new
// index its partitioning in the same plan; a Go source states no index
// setting this provider reads, so that part is not ported.
func TestHostAddsAnIndexToATableWithRows_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	output, err := hostApply(c, directory, hostGlobalItems(directory, "idx_items_kind", ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, sku, kind, price) VALUES (1, 'a'u, 'tool'u, 10)"), qt.IsNil)

	withPrice := hostGlobalItems(directory, "idx_items_kind", "\t//ptah:schema:index name=\"idx_items_price\" fields=\"price\" type=\"async\" include=\"kind,sku\"\n")
	for _, run := range []struct{ name, want string }{
		{"the index added", `(?s).*row-table-alter-operation.*Schema apply completed successfully.*`},
		{"a second run", `(?s).*Schema is synced, no changes to be made.*`},
		{"a third run", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, withPrice)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(heldIndexKinds(c, opened, absolute)["idx_items_price"], qt.Equals, options.IndexTypeGlobalAsync)
	c.Assert(heldIndexColumns(c, opened, absolute)["idx_items_price"], qt.DeepEquals, [2][]string{{"price"}, {"kind", "sku"}})
}

// The host's TestYDBConnection_ClusterWithoutMoveIndex_HappyPath. On a
// cluster that turned EnableMoveIndex off, which refuses ALTER TABLE ...
// RENAME INDEX, a session reports no index_rename, and an index the
// declaration renames is planned as a drop and an add, which the cluster
// takes. The table keeps its rows, the next run is synced, and the index the
// table holds is the declared one under its new name. With the flag on, a
// host that lists planning.index-renames renames the index in place instead
// (TestHostRenamesAnIndexInPlace_HappyPath).
func TestHostRenamesAnIndexAsADropAndAnAdd_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableMoveIndex": false})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Capabilities["index_rename"], qt.IsFalse)

	output, err := hostApply(c, directory, hostGlobalItems(directory, "idx_items_kind", ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, sku, kind, price) VALUES (1, 'a'u, 'tool'u, 10)"), qt.IsNil)
	renamed := hostGlobalItems(directory, "idx_items_by_kind", "")
	for _, run := range []struct{ name, want string }{
		// The plan prints the removal's reason, then the addition's operation.
		{"the rename", `(?s).*Drops a secondary index.*row-table-alter-operation.*Schema apply completed successfully.*`},
		{"a run after the rename", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, renamed)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	held := heldIndexColumns(c, opened, absolute)
	c.Assert(held["idx_items_by_kind"], qt.DeepEquals, [2][]string{{"kind"}, nil})
	_, kept := held["idx_items_kind"]
	c.Assert(kept, qt.IsFalse)
	var count uint64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+absolute+"` VIEW `idx_items_by_kind` WHERE kind = 'tool'u").Scan(&count), qt.IsNil)
	c.Assert(count, qt.Equals, uint64(1))
}

// Ported from the host's TestYDBWriter_ClusterWithoutMoveIndex_FailurePath. A
// RENAME INDEX that reaches a cluster with EnableMoveIndex off, as one does
// from a migration file, is refused by the server: the provider reports it as
// not applied, and the index keeps its name. To a client that lists
// errors.reasons the error names the capability and the feature flag, in the
// words of the host's embedded engine, and leaves out the server's text,
// which the result's message keeps. The test address names the monitoring
// endpoint, so the session read the flags, and the error asks only to turn
// the flag on.
func TestRawIndexRenameWithoutMoveIndex_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableMoveIndex": false})
	output, err := hostApply(c, directory, hostGlobalItems(directory, "idx_items_kind", ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	client := startProviderFor(c, provider.ErrorReasonsFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })

	result, err := executeFailing(c, client, session.Id, "ALTER TABLE `"+absolute+"` RENAME INDEX `idx_items_kind` TO `idx_items_by_kind`")

	c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
	c.Assert(result.GetDiagnosticCode(), qt.Equals, "ydb.statement-rejected")
	c.Assert(err, qt.ErrorMatches, `provider execution: NOT_APPLIED \(0 units confirmed applied\): capability index_rename is off on this YDB cluster, `+
		`which runs with feature flag EnableMoveIndex off: YDB refused the statement, saying "Move index is not supported yet" \(PRECONDITION_FAILED[^)]*\)\. Turn the flag on`)
	c.Assert(result.GetMessage(), qt.Matches, `(?s).*Move index is not supported yet.*`)
	held := heldIndexColumns(c, opened, absolute)
	c.Assert(held["idx_items_kind"], qt.DeepEquals, [2][]string{{"kind"}, nil})
}
