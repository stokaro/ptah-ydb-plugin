//go:build integration

package integration_test

import (
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// feedEvents declares table events in directory with a changefeed feed kept
// for retention and the indexes that follow.
func feedEvents(directory, retention, indexes string) string {
	return "tables:\n  events:\n    schema: " + directory + "\n" +
		"    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      payload: {type: Utf8}\n      category: {type: Utf8}\n" +
		"    indexes:\n" + indexes +
		"    changefeeds:\n      feed:\n        mode: UPDATES\n        format: JSON\n        retention_period: " + retention + "\n"
}

// Ported from the host's TestYDBIndexAndFeatureRollbackPreservesSkippedIndex:
// a project that skips index drops (`diff.skip: [drop_index]`) generates a
// migration that renames index old_name to new_name with a new comment and
// shortens the changefeed's retention, and leaves index retained, which the
// declaration no longer names, in place with its comment. `migrations up`
// applies it and nothing is left to plan; `migrations down` takes the
// rename, the comment and the retention back, the first declaration then
// plans nothing, and retained keeps its comment throughout.
func TestHostRollsBackAnIndexAndAFeatureKeepingASkippedIndex_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableMoveIndex": true})
	absolute := path.Join(database, directory, "events")
	before := feedEvents(directory, "PT24H",
		"      retained: {fields: [payload], comment: retained comment}\n      old_name: {fields: [payload, category], comment: old comment}\n")
	after := feedEvents(directory, "PT12H", "      new_name: {fields: [payload, category], comment: new comment}\n")
	hostRun(c, directory, "the table", before, `(?s).*Schema apply completed successfully.*`)
	config := filepath.Join(c.TempDir(), "ptah.yaml")
	c.Assert(os.WriteFile(config, []byte("diff:\n  skip: [drop_index]\n"), 0o600), qt.IsNil)

	migrations, up, down := hostGenerate(c, directory, "yaml", after, "--config", config)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	forwardIndexes, forwardAttributes := describedItems(c, opened, absolute)
	settled := hostPlanned(c, directory, "yaml", after, "--config", config)
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	indexes, attributes := describedItems(c, opened, absolute)

	c.Assert(up, qt.Not(qt.Contains), "DROP INDEX `retained`")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s\n%s", applied, up))
	c.Assert(slices.Sorted(maps.Keys(forwardIndexes)), qt.DeepEquals, []string{"new_name", "retained"})
	c.Assert(forwardAttributes["ptah.comment.index.retained"], qt.Equals, "retained comment")
	c.Assert(forwardAttributes["ptah.comment.index.new_name"], qt.Equals, "new comment")
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(slices.Sorted(maps.Keys(indexes)), qt.DeepEquals, []string{"old_name", "retained"})
	c.Assert(attributes["ptah.comment.index.retained"], qt.Equals, "retained comment")
	c.Assert(attributes["ptah.comment.index.old_name"], qt.Equals, "old comment")
	c.Assert(hostPlanned(c, directory, "yaml", before), qt.HasLen, 0)
}
