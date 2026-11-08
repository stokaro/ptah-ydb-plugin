//go:build integration

package integration_test

import (
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func indexCommentDocument(index, comment string) string {
	return "CREATE TABLE `docs` (id Uint64 NOT NULL, a Utf8, PRIMARY KEY (id)" + index + ");\n" + comment
}

func indexCommentDiff(c *qt.C, address, from, to string) string {
	c.Helper()
	files := writeFiles(c, c.TempDir(), map[string]string{"from.sql": from, "to.sql": to})
	out, err := hostCommand(c, "schema", "diff", "--from", filepath.Join(files, "from.sql"),
		"--to", filepath.Join(files, "to.sql"), "--dev-url", address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	return out
}

func indexCommentStatements(sql string) []string {
	var result []string
	for line := range strings.SplitSeq(sql, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "COMMENT ON INDEX ") {
			result = append(result, line)
		}
	}
	return result
}

// Adding, changing and removing a comment on a kept index writes only its
// table attribute. The index itself is not rebuilt. This ports the three
// TestIndexes_YDBCommentChangesInPlace cases through the host.
func TestHostChangesKeptIndexCommentsInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostChange(c, selfDiffIndexChange)
	realm := newCLIRealm(c)
	const index = ", INDEX by_a GLOBAL ON (a)"
	for _, test := range []struct {
		name, oldComment, newComment, want string
	}{
		{"comment declared", "", "COMMENT ON INDEX by_a ON `docs` IS 'Lookup';\n", "COMMENT ON INDEX `by_a` ON `docs` IS 'Lookup';"},
		{"comment changed", "COMMENT ON INDEX by_a ON `docs` IS 'Lookup';\n", "COMMENT ON INDEX by_a ON `docs` IS 'By name';\n",
			"COMMENT ON INDEX `by_a` ON `docs` IS 'By name';"},
		{"comment removed", "COMMENT ON INDEX by_a ON `docs` IS 'Lookup';\n", "", "COMMENT ON INDEX `by_a` ON `docs` IS NULL;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			out := indexCommentDiff(c, realm.address, indexCommentDocument(index, test.oldComment), indexCommentDocument(index, test.newComment))
			c.Assert(indexCommentStatements(out), qt.DeepEquals, []string{test.want}, qt.Commentf("%s", out))
			c.Assert(out, qt.Not(qt.Contains), "DROP INDEX")
			c.Assert(out, qt.Not(qt.Contains), "ADD INDEX")
		})
	}
}

// A rename moves an unchanged or changed comment to the new attribute key,
// clears a removed comment, and writes none if neither side has one. This
// ports TestIndexes_YDBCommentMovesWithARename through the host.
func TestHostMovesIndexCommentsWithRename_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	requireHostChange(c, documentIndexRenameChange)
	realm := newCLIRealm(c)
	for _, test := range []struct {
		name, oldComment, newComment string
		want                         []string
	}{
		{"same comment", "Lookup", "Lookup", []string{
			"COMMENT ON INDEX `old` ON `docs` IS NULL;", "COMMENT ON INDEX `new` ON `docs` IS 'Lookup';"}},
		{"changed comment", "Old", "New", []string{
			"COMMENT ON INDEX `old` ON `docs` IS NULL;", "COMMENT ON INDEX `new` ON `docs` IS 'New';"}},
		// The new name's key is cleared too: an earlier use of that name could
		// have left a stale comment attribute on the table.
		{"removed comment", "Old", "", []string{
			"COMMENT ON INDEX `old` ON `docs` IS NULL;", "COMMENT ON INDEX `new` ON `docs` IS NULL;"}},
		{"no comment", "", "", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			from := indexCommentDocument(", INDEX old GLOBAL ON (a)", indexCommentClause("old", test.oldComment))
			to := indexCommentDocument(", INDEX new GLOBAL ON (a)", indexCommentClause("new", test.newComment))
			out := indexCommentDiff(c, realm.address, from, to)
			c.Assert(out, qt.Contains, "RENAME INDEX `old` TO `new`")
			c.Assert(indexCommentStatements(out), qt.DeepEquals, test.want, qt.Commentf("%s", out))
			c.Assert(out, qt.Not(qt.Contains), "DROP INDEX")
			c.Assert(out, qt.Not(qt.Contains), "ADD INDEX")
		})
	}
}

func indexCommentClause(name, comment string) string {
	if comment == "" {
		return ""
	}
	return "COMMENT ON INDEX " + name + " ON `docs` IS '" + comment + "';\n"
}

// Dropping an index clears its comment. Rebuilding a changed index under the
// same name clears an omitted comment or writes the new one on the replacement.
// This ports the drop and rebuild cases of TestIndexes_YDBCommentOfADroppedIndex.
func TestHostChangesCommentsOfDroppedAndRebuiltIndexes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	from := indexCommentDocument(", INDEX by_a GLOBAL ON (a)", indexCommentClause("by_a", "Old"))
	for _, test := range []struct {
		name, index, comment, wantComment string
		wantAdds                          int
	}{
		{"dropped", "", "", "COMMENT ON INDEX `by_a` ON `docs` IS NULL;", 0},
		{"rebuilt without comment", ", INDEX by_a GLOBAL ASYNC ON (a)", "", "COMMENT ON INDEX `by_a` ON `docs` IS NULL;", 1},
		{"rebuilt with new comment", ", INDEX by_a GLOBAL ASYNC ON (a)", "New", "COMMENT ON INDEX `by_a` ON `docs` IS 'New';", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			to := indexCommentDocument(test.index, indexCommentClause("by_a", test.comment))
			out := indexCommentDiff(c, realm.address, from, to)
			c.Assert(out, qt.Contains, "DROP INDEX `by_a`")
			c.Assert(strings.Count(out, "ADD INDEX `by_a`"), qt.Equals, test.wantAdds)
			c.Assert(indexCommentStatements(out), qt.DeepEquals, []string{test.wantComment}, qt.Commentf("%s", out))
		})
	}
}

// The old host's rebuild case makes the replacement unique. A 25.1 target
// cannot add a unique index to an existing table, so this exact case runs on
// a target that advertises unique_index_on_existing_table.
func TestHostChangesCommentsWhenAnIndexBecomesUnique_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireCapability(c, targetCapabilities(c), tablecreate.UniqueIndexOnExistingTableCapability)
	realm := newCLIRealm(c)
	from := indexCommentDocument(", INDEX by_a GLOBAL ON (a)", indexCommentClause("by_a", "Old"))
	for _, test := range []struct {
		name, comment, wantComment string
	}{
		{"without comment", "", "COMMENT ON INDEX `by_a` ON `docs` IS NULL;"},
		{"with new comment", "New", "COMMENT ON INDEX `by_a` ON `docs` IS 'New';"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			to := indexCommentDocument(", INDEX by_a GLOBAL UNIQUE ON (a)", indexCommentClause("by_a", test.comment))
			out := indexCommentDiff(c, realm.address, from, to)
			c.Assert(out, qt.Contains, "DROP INDEX `by_a`")
			c.Assert(out, qt.Contains, "ADD INDEX `by_a` GLOBAL UNIQUE")
			c.Assert(indexCommentStatements(out), qt.DeepEquals, []string{test.wantComment}, qt.Commentf("%s", out))
		})
	}
}
