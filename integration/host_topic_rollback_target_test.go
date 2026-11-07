//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/topic/topictypes"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A down migration restores the retention period YDB held before the
// forward change. It keeps the partitions the change added and pauses the
// auto-partitioning the change enabled, because YDB cannot undo either in
// place. The generated down file reports both limits, and the server reaches
// the same state. This ports all three cases of the host's
// ydbtopic.TestRollbackTarget through a migration and a live topic.
func TestHostRollsBackATopicToItsReachableState_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "events")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TOPIC `"+absolute+"` WITH (min_active_partitions = 1, retention_period = Interval('P1D'))"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TOPIC `"+absolute+"`"), qt.IsNil)
	})
	after := "topics:\n  events:\n    schema: " + directory + "\n    min_active_partitions: 3\n    max_active_partitions: 6\n" +
		"    auto_partitioning_strategy: scale_up\n    retention_period: P2D\n"
	migrations, up, down := hostGenerate(c, directory, "yaml", after)
	c.Assert(up, qt.Contains, "auto_partitioning_strategy = 'scale_up'")
	c.Assert(down, qt.Contains, "auto_partitioning_strategy = 'paused'")
	c.Assert(down, qt.Contains, "min_active_partitions = 3")
	c.Assert(down, qt.Contains, "retention_period = Interval('P1D')")
	c.Assert(down, qt.Contains, "keeps the 3 partitions")
	c.Assert(down, qt.Contains, "keeps auto-partitioning, paused")

	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s\n%s", applied, up))
	forward, err := opened.Driver.Topic().Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	c.Assert(forward.PartitionSettings.MinActivePartitions, qt.Equals, int64(3))
	c.Assert(forward.PartitionSettings.AutoPartitioningSettings.AutoPartitioningStrategy, qt.Equals, topictypes.AutoPartitioningStrategyScaleUp)
	c.Assert(forward.RetentionPeriod, qt.Equals, 48*time.Hour)

	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	c.Assert(downErr, qt.IsNil, qt.Commentf("%s\n%s", rolledBack, down))
	restored, err := opened.Driver.Topic().Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	c.Assert(restored.PartitionSettings.MinActivePartitions, qt.Equals, int64(3))
	c.Assert(restored.PartitionSettings.MaxActivePartitions, qt.Equals, int64(6))
	c.Assert(restored.PartitionSettings.AutoPartitioningSettings.AutoPartitioningStrategy, qt.Equals, topictypes.AutoPartitioningStrategyPaused)
	c.Assert(restored.RetentionPeriod, qt.Equals, 24*time.Hour)
}
