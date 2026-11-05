//go:build integration

package integration_test

import (
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// topicMigrationSteps are a topic's creation, its change and its drop, each a
// schema file of a step, and the statement the host's embedded engine wrote
// into that step's up file, recorded from 74de0b65b on 26.2.1.14.
var topicMigrationSteps = []struct{ schema, up string }{
	{"topics:\n  events:\n    min_active_partitions: 1\n    consumers:\n      worker: {}\n",
		"CREATE TOPIC `events` (CONSUMER `worker`) WITH (min_active_partitions = 1);"},
	{"topics:\n  events:\n    min_active_partitions: 1\n    retention_period: P2D\n    consumers:\n      worker: {}\n      audit: {important: true}\n",
		"ALTER TOPIC `events` SET (min_active_partitions = 1, auto_partitioning_strategy = 'disabled', retention_period = Interval('P2D'), " +
			"partition_write_speed_bytes_per_second = 1048576, partition_write_burst_bytes = 1048576, supported_codecs = ''), ADD CONSUMER `audit` WITH (important = TRUE);"},
	{"topics: {}\n", "DROP TOPIC `events`;"},
}

// topicMigrationDowns are what the host's embedded engine wrote into the down
// files of those steps: the statement that undoes each and the recovery limit
// it names.
var topicMigrationDowns = []string{
	"-- Recovery limit: \"dropping topic events discards every message written to it and every consumer's position\"\nDROP TOPIC `events`;",
	"-- Recovery limit: \"consumer \\\"audit\\\" the change added to topic events is dropped by the rollback, with its position\"\n" +
		"ALTER TOPIC `events` SET (min_active_partitions = 1, auto_partitioning_strategy = 'disabled', retention_period = Interval('P1D'), " +
		"partition_write_speed_bytes_per_second = 1048576, partition_write_burst_bytes = 1048576, supported_codecs = ''), DROP CONSUMER `audit`;",
	"-- Recovery limit: \"the messages topic events held and its consumers' positions were dropped; the rollback creates it empty\"\n" +
		"CREATE TOPIC `events` (",
}

// `migrations generate` writes a topic's creation, change and drop into
// migration files through this provider, with the statements the host's
// embedded engine wrote, and `migrations up` and `migrations down` run them.
// A topic operation had no text form, so the host refused each generation
// with `no text form for operation .../topic-operation`. The run is a dev
// realm of its own, whose lock node is at the realm's root.
func TestHostWritesTopicMigrations_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	migrations := c.TempDir()
	for i, step := range topicMigrationSteps {
		file := filepath.Join(writeFiles(c, c.TempDir(), map[string]string{"schema.yaml": step.schema}), "schema.yaml")
		generated, err := hostCommand(c, "migrations", "generate", "--db-url", realm.address, "--schema-file", file,
			"--migrations-dir", migrations, "--name", "topic")
		c.Assert(err, qt.IsNil, qt.Commentf("step %d, migrations generate:\n%s", i+1, generated))
		applied, err := hostCommand(c, "migrations", "up", "--db-url", realm.address, "--migrations-dir", migrations, "--allow-destructive")
		c.Assert(err, qt.IsNil, qt.Commentf("step %d, migrations up:\n%s", i+1, applied))
	}
	up, down := generatedPair(c, migrations)
	_, existsAfterUp := describedTopic(c, realm.opened, realm.root, "", "events")

	rolledBack, downErr := hostCommand(c, "migrations", "down", "--db-url", realm.address, "--migrations-dir", migrations, "--target", "0", "--confirm")
	_, existsAfterDown := describedTopic(c, realm.opened, realm.root, "", "events")

	for _, step := range topicMigrationSteps {
		c.Assert(up, qt.Contains, step.up)
	}
	for _, statement := range topicMigrationDowns {
		c.Assert(down, qt.Contains, statement)
	}
	c.Assert(existsAfterUp, qt.IsFalse)
	c.Assert(downErr, qt.IsNil, qt.Commentf("migrations down:\n%s", rolledBack))
	c.Assert(existsAfterDown, qt.IsFalse)
}
