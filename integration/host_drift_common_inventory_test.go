//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// inventoryDocument declares, in directory, the kinds of the host's inventory
// document that YDB holds: two tables with their columns, an index named
// index, and the group group.
func inventoryDocument(directory, index, group string) string {
	return "tables:\n" +
		"  users:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      email: {type: Utf8}\n" +
		"    indexes:\n      " + index + ": {fields: [email]}\n" +
		"  posts:\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      author_id: {type: Int64}\n      title: {type: Utf8}\n" +
		"roles:\n  " + group + ":\n    group: true\n"
}

// commonInventory runs `schema drift --format json` over source and returns
// the report's verdict and the entries of the kinds the host pairs itself,
// leaving out the provider's own.
func commonInventory(c *qt.C, directory, source string) (bool, []inventoryEntry) {
	c.Helper()
	stdout, stderr, exit := hostVerb(c, "schema", "drift", "--format", "json", "--schemas", directory, "--schema-file", writeSource(c, "desired.yaml", source))
	c.Assert(exit == 0 || exit == 1, qt.IsTrue, qt.Commentf("exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr))
	var report struct {
		Drift     bool             `json:"drift"`
		Inventory []inventoryEntry `json:"inventory"`
	}
	c.Assert(json.Unmarshal([]byte(stdout), &report), qt.IsNil, qt.Commentf("stdout:\n%s", stdout))
	var common []inventoryEntry
	for _, entry := range report.Inventory {
		if !strings.HasPrefix(entry.Kind, schema.ProviderID+"/") {
			common = append(common, entry)
		}
	}
	return report.Drift, common
}

// inventoryFixture applies inventoryDocument with the index
// idx_users_email to a fixture directory and returns the directory and the
// group the document declares.
func inventoryFixture(c *qt.C) (opened *connection.Connection, directory, group string) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	group = "ptahinv" + strings.ToLower(rand.Text())[:8]
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP GROUP IF EXISTS `"+group+"`"), qt.IsNil)
	})
	applied, err := hostApplySource(c, directory, "yaml", inventoryDocument(directory, "idx_users_email", group))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", applied))
	return opened, directory, group
}

// Ported from the YDB row of the host's
// TestCompareSchemas_ADocumentAgainstItselfCountsEveryKindAlike: a document
// checked against the database it was applied to finds no drift, and the
// drift report counts every kind the host pairs the same on both sides. The
// host's document also held a foreign key, a CHECK, an enum type and a
// function, none of which YDB has, so a YDB database holds the rest: tables,
// columns, an index and a role.
func TestHostDriftCountsEveryCommonKindAlike_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, group := inventoryFixture(c)

	drift, inventory := commonInventory(c, directory, inventoryDocument(directory, "idx_users_email", group))

	c.Assert(drift, qt.IsFalse)
	c.Assert(inventory, qt.DeepEquals, []inventoryEntry{
		{Kind: "columns", Desired: 5, Observed: 5},
		{Kind: "indexes", Desired: 1, Observed: 1},
		{Kind: "roles", Desired: 1, Observed: 1},
		{Kind: "tables", Desired: 2, Observed: 2},
	})
}

// Ported from the host's TestIndexes_YDBCountsARenamedIndexAsAPair: an index
// the declaration keeps under another name is one object on each side,
// neither added nor removed. The host keeps its matched count out of every
// report except the drift inventory, where the index counts once on each
// side, and the migration renames it rather than dropping and creating it.
// The host pairs the two names only for a provider it negotiates index
// renames with, on a cluster that renames an index in place.
func TestHostCountsARenamedIndexAsAPair_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	opened, directory, group := inventoryFixture(c)
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableMoveIndex": true})
	renamed := inventoryDocument(directory, "by_email", group)

	drift, inventory := commonInventory(c, directory, renamed)
	_, up, _ := hostGenerate(c, directory, "yaml", renamed)

	c.Assert(drift, qt.IsTrue)
	c.Assert(inventory, qt.Contains, inventoryEntry{Kind: "indexes", Desired: 1, Observed: 1})
	c.Assert(up, qt.Contains, "RENAME INDEX `idx_users_email` TO `by_email`")
	c.Assert(up, qt.Not(qt.Contains), "DROP INDEX")
}
