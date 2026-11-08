//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// checkpointFeaturePair replays a migration directory on the host's shadow
// database and returns the cumulative up/down pair it writes.
func checkpointFeaturePair(c *qt.C, files map[string]string) (up, down string) {
	c.Helper()
	_, _, _ = lockFixture(c)
	migrations := writeFiles(c, c.TempDir(), files)
	hashed, err := hostOffline(c, "migrations", "hash", "--dir", migrations)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", hashed))
	written, err := hostCommand(c, "migrations", "checkpoint", "--migrations-dir", migrations, "--shadow-db", os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", written))
	read := func(suffix string) string {
		c.Helper()
		paths, err := filepath.Glob(filepath.Join(migrations, "*.checkpoint."+suffix+".sql"))
		c.Assert(err, qt.IsNil)
		c.Assert(paths, qt.HasLen, 1)
		body, err := os.ReadFile(paths[0])
		c.Assert(err, qt.IsNil)
		return string(body)
	}
	return read("up"), read("down")
}

// A checkpoint of a database with no user objects has no YDB feature
// statements in either direction. The read must establish their absence.
func TestHostCheckpointEmptyYDBHasKnownFeatureAbsence_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	up, down := checkpointFeaturePair(c, map[string]string{
		"0000000001_create.up.sql":   "CREATE TABLE `marker` (`id` Int32 NOT NULL, PRIMARY KEY (`id`));\n",
		"0000000001_create.down.sql": "DROP TABLE `marker`;\n",
		"0000000002_drop.up.sql":     "DROP TABLE `marker`;\n",
		"0000000002_drop.down.sql":   "CREATE TABLE `marker` (`id` Int32 NOT NULL, PRIMARY KEY (`id`));\n",
	})
	c.Assert(up, qt.Equals, "")
	c.Assert(down, qt.Equals, "")
}

// A bidirectional migration from an empty YDB schema selects the owners of a
// coordination node and a streaming query for both the forward and reverse
// files. The stopped query reads and writes topics declared in the same source.
func TestHostGenerateStandaloneYDBFeaturesUsesSelectedOwners_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, _, _ := streamingFixture(c)
	declared := "coordination_nodes:\n  locks:\n    schema: " + directory + "\n" +
		"topics:\n  source:\n    schema: " + directory + "\n  sink:\n    schema: " + directory + "\n" +
		"streaming_queries:\n  copy:\n    schema: " + directory + "\n" +
		"    text: \"INSERT INTO `" + directory + "/sink` SELECT * FROM `" + directory + "/source`;\"\n    run: false\n"

	_, up, down := hostGenerate(c, directory, "yaml", declared)
	c.Assert(up, qt.Contains, "CREATE COORDINATION NODE `"+directory+"/locks`")
	c.Assert(up, qt.Contains, "CREATE STREAMING QUERY `"+directory+"/copy`")
	c.Assert(down, qt.Contains, "DROP STREAMING QUERY `"+directory+"/copy`")
	c.Assert(down, qt.Contains, "DROP COORDINATION NODE `"+directory+"/locks`")
}

// A checkpoint replays a standalone coordination node in its shadow database,
// then selects that node's owner in both cumulative files. The empty baseline
// must retain the shadow database path to bind the observed native object.
func TestHostCheckpointStandaloneCoordinationUsesSelectedOwner_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostChange(c, "stokaro/ptah#5788, which binds an empty checkpoint baseline to its database")
	up, down := checkpointFeaturePair(c, map[string]string{
		"0000000001_create.up.sql":   "CREATE COORDINATION NODE `locks`;\n",
		"0000000001_create.down.sql": "DROP COORDINATION NODE `locks`;\n",
	})
	c.Assert(up, qt.Contains, "CREATE COORDINATION NODE `locks`")
	c.Assert(down, qt.Contains, "DROP COORDINATION NODE `locks`")
}
