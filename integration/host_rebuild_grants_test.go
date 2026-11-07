//go:build integration

package integration_test

import (
	"path"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// principalEntries are the access control entries the object at absolute holds
// for the run's principals, as a description of its path reports them, each
// as the subject and the permission, sorted. They are read without a provider
// session.
func principalEntries(c *qt.C, opened *connection.Connection, absolute string, names accessNames) []string {
	c.Helper()
	var entries []string
	for _, entry := range rootPermissions(c, opened, absolute) {
		for _, permission := range entry.PermissionNames {
			if slices.Contains(names.all(), entry.Subject) {
				entries = append(entries, entry.Subject+" "+permission)
			}
		}
	}
	slices.Sort(entries)
	return entries
}

// grantedItems is countedItems with the run's group and a blocked user, the
// group holding privileges on the table and the user LIST.
func grantedItems(directory, count string, names accessNames, privileges ...string) string {
	return countedItems(directory, count) +
		"roles:\n  " + names.group + ":\n    group: true\n  " + names.blocked + ":\n    login: false\n" +
		"grants:\n  rows:\n    role: " + names.group + "\n    privileges: [" + strings.Join(privileges, ", ") + "]\n    on_table: " + directory + ".items\n" +
		"  listing:\n    role: " + names.blocked + "\n    privileges: [LIST]\n    on_table: " + directory + ".items\n"
}

// A table the plan rebuilds gets back every permission entry the old table
// held, for every subject, except the one the plan revokes: the new table is
// created with the permissions of its directory, so nothing else gives them
// back, and the next run plans nothing. Ported from the host's
// TestGenerateMigrationAST_RebuildRegrants_HappyPath.
func TestHostRebuildGivesBackTheTablesGrants_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	applyRuns(c, directory, "yaml", grantedItems(directory, "Int32", names, "SELECT ROW", "ERASE ROW"))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (id, n) VALUES (1ul, 7)"), qt.IsNil)

	applyUntilSynced(c, directory, "yaml", grantedItems(directory, "Int64", names, "SELECT ROW"), "--allow-table-rebuild")

	c.Assert(heldColumnTypes(c, opened, absolute)["n"], qt.Equals, "Optional<Int64>")
	c.Assert(rowCount(c, opened, absolute, "n = 7l"), qt.Equals, int64(1))
	c.Assert(principalEntries(c, opened, absolute, names), qt.DeepEquals, []string{names.blocked + " ydb.generic.list", names.group + " ydb.granular.select_row"})
}

// keptAndOther declares tables kept and other of the directory with the run's
// group, and grants the group ERASE ROW on each table named in erasable.
func keptAndOther(directory, other string, names accessNames, erasable ...string) string {
	document := "tables:\n"
	for _, table := range []string{"kept", other} {
		document += "  " + table + ":\n    schema: " + directory + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n"
	}
	document += "roles:\n  " + names.group + ":\n    group: true\n"
	if len(erasable) == 0 {
		return document
	}
	document += "grants:\n"
	for _, table := range erasable {
		document += "  " + table + ":\n    role: " + names.group + "\n    privileges: [ERASE ROW]\n    on_table: " + directory + "." + table + "\n"
	}
	return document
}

// A plan revokes a permission only on a table that stays: a table the plan
// drops takes its entries with it, and one it creates holds none yet. Ported
// from the host's TestGenerateMigrationAST_RevokesOnlyWhereTheTableStays.
func TestHostRevokesOnlyOnATableThatStays_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	applyRuns(c, directory, "yaml", keptAndOther(directory, "legacy", names, "kept", "legacy"))
	desired := strings.Replace(keptAndOther(directory, "orders", names), "  legacy:", "  orders:", 1)

	planned := hostPlanned(c, directory, "yaml", desired)
	applyRuns(c, directory, "yaml", desired)

	c.Assert(planned, qt.DeepEquals, []string{
		"REVOKE 'ydb.granular.erase_row' ON `" + directory + "/kept` FROM `" + names.group + "`",
		"row-table-create-operation " + directory + ".orders",
		"table-drop-operation " + directory + ".legacy",
	})
	c.Assert(principalEntries(c, opened, path.Join(database, directory, "kept"), names), qt.HasLen, 0)
}
