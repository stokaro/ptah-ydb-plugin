//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The recovery limits a changefeed rollback states.
const (
	readdedFeedLimit = `-- Recovery limit: "a changefeed of table %s the rollback adds again starts a new stream; the messages and the consumers' positions of the dropped one are lost"`
	addedFeedLimit   = `-- Recovery limit: "a changefeed the forward change added to table %s is dropped by the rollback with its topic; its unread messages and consumer positions are lost"`
	expiredLimit     = `-- Recovery limit: "Restoring retention cannot recover messages that have already expired."`
)

// Ported from the host's TestGeneratorUsesOwnerReversalAndReportsRecoveryLimits
// (migration/generator/internal/featuretests): a migration that swaps one
// changefeed for another beside a third it keeps rolls back by dropping the
// new one and adding the old one again with its consumer, says what neither
// recovers, and leaves the kept one out of both files. Both files apply. The
// statements are the built-in engine's at 74de0b65b on 25.1.4.7 and 26.2.1.14.
// The host test kept the third changefeed by leaving it undescribed; this
// provider's sources describe a table's changefeeds as one set, so the
// source declares it as it stands.
func TestHostRollsBackAChangefeedSwapLeavingItsSibling_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute, table := path.Join(database, directory, "items"), path.Join(directory, "items")
	retained := "      retained:\n        mode: UPDATES\n        format: JSON\n"
	before := rollbackFeeds(directory, "      gone:\n        mode: UPDATES\n        format: JSON\n        consumers:\n          reader: {}\n"+retained)
	after := rollbackFeeds(directory, "      fresh:\n        mode: KEYS_ONLY\n        format: JSON\n"+retained)
	created, createErr := hostApplySource(c, directory, "yaml", before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))

	migrations, up, down := hostGenerate(c, directory, "yaml", after)
	upped, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	downed, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, "ALTER TABLE `"+table+"` DROP CHANGEFEED `gone`;\nALTER TABLE `"+table+"` ADD CHANGEFEED `fresh` WITH (MODE = 'KEYS_ONLY', FORMAT = 'JSON');")
	c.Assert(down, qt.Contains, "ALTER TABLE `"+table+"` DROP CHANGEFEED `fresh`;\nALTER TABLE `"+table+"` ADD CHANGEFEED `gone` WITH (MODE = 'UPDATES', FORMAT = 'JSON');\n"+
		"ALTER TOPIC `"+table+"/gone` ADD CONSUMER `reader`;")
	c.Assert(down, qt.Contains, strings.ReplaceAll(addedFeedLimit, "%s", table))
	c.Assert(down, qt.Contains, strings.ReplaceAll(readdedFeedLimit, "%s", table))
	c.Assert(up+down, qt.Not(qt.Contains), "retained")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", upped))
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", downed))
	feeds, found := describedChangefeeds(c, opened, absolute)
	c.Assert(found, qt.IsTrue)
	c.Assert(feeds, qt.DeepEquals, map[string]feedShape{
		"gone":     {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour, Consumers: []string{"reader"}},
		"retained": {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour},
	})
}

// Ported from the host's TestGeneratorProjectsImplicitStateFromTheOwner: a
// migration that shortens a changefeed's retention and stops stating the four
// partitions its topic holds changes the retention alone, since YDB never
// removes a partition, and its rollback puts the retention back from what the
// change leaves, four partitions included, so it writes no partition setting
// either. Both files apply and the topic keeps its four partitions. The
// statements and the limit are the built-in engine's at 74de0b65b on
// 25.1.4.7 and 26.2.1.14; this provider said the rollback added a stream
// again, where it changes the topic in place (G234).
func TestHostRollsBackAChangefeedRetentionKeepingItsPartitions_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute, table := path.Join(database, directory, "items"), path.Join(directory, "items")
	for _, statement := range []string{
		"CREATE TABLE `" + absolute + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"ALTER TABLE `" + absolute + "` ADD CHANGEFEED `updates` WITH (MODE = 'UPDATES', FORMAT = 'JSON', TOPIC_MIN_ACTIVE_PARTITIONS = 4)",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	declared := rollbackFeeds(directory, "      updates:\n        mode: UPDATES\n        format: JSON\n        retention_period: PT12H\n")
	partitions := func() int64 {
		c.Helper()
		described, err := opened.Driver.Topic().Describe(c.Context(), path.Join(absolute, "updates"))
		c.Assert(err, qt.IsNil)
		return described.PartitionSettings.MinActivePartitions
	}
	retention := func() time.Duration {
		c.Helper()
		feeds, found := describedChangefeeds(c, opened, absolute)
		c.Assert(found, qt.IsTrue)
		return feeds["updates"].Retention
	}

	migrations, up, down := hostGenerate(c, directory, "yaml", declared)
	upped, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	upPartitions, upRetention := partitions(), retention()
	downed, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, "ALTER TOPIC `"+table+"/updates` SET (retention_period = Interval('PT12H'));")
	c.Assert(down, qt.Contains, "ALTER TOPIC `"+table+"/updates` SET (retention_period = Interval('P1D'));")
	c.Assert(down, qt.Contains, expiredLimit)
	c.Assert(down, qt.Not(qt.Contains), "starts a new stream")
	c.Assert(strings.ToLower(up+down), qt.Not(qt.Contains), "partition")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", upped))
	c.Assert(upPartitions, qt.Equals, int64(4))
	c.Assert(upRetention, qt.Equals, 12*time.Hour)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", downed))
	c.Assert(partitions(), qt.Equals, int64(4))
	c.Assert(retention(), qt.Equals, 24*time.Hour)
}
