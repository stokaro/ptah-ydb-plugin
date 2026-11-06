//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

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
		"CREATE TOPIC `events` (CONSUMER `worker`, CONSUMER `audit` WITH (important = TRUE)) WITH (min_active_partitions = 1, auto_partitioning_strategy = 'disabled', " +
		"retention_period = Interval('P2D'), partition_write_speed_bytes_per_second = 1048576, partition_write_burst_bytes = 1048576);",
}

// topicMigrations generates a migration for each schema file of schemas in
// turn and runs it on realm, and returns the up and down files and the outcome
// of `migrations down` to version 0. The topic events exists after neither.
func topicMigrations(c *qt.C, realm cliRealm, schemas []string) (up, down, rolledBack string, downErr error) {
	c.Helper()
	migrations := c.TempDir()
	for i, schema := range schemas {
		file := filepath.Join(writeFiles(c, c.TempDir(), map[string]string{"schema.yaml": schema}), "schema.yaml")
		generated, err := hostCommand(c, "migrations", "generate", "--db-url", realm.address, "--schema-file", file,
			"--migrations-dir", migrations, "--name", "topic")
		c.Assert(err, qt.IsNil, qt.Commentf("step %d, migrations generate:\n%s", i+1, generated))
		applied, err := hostCommand(c, "migrations", "up", "--db-url", realm.address, "--migrations-dir", migrations, "--allow-destructive")
		c.Assert(err, qt.IsNil, qt.Commentf("step %d, migrations up:\n%s", i+1, applied))
	}
	up, down = generatedPair(c, migrations)
	_, existsAfterUp := describedTopic(c, realm.opened, realm.root, "", "events")
	c.Assert(existsAfterUp, qt.IsFalse)
	rolledBack, downErr = hostCommand(c, "migrations", "down", "--db-url", realm.address, "--migrations-dir", migrations, "--target", "0", "--confirm")
	_, existsAfterDown := describedTopic(c, realm.opened, realm.root, "", "events")
	c.Assert(existsAfterDown, qt.IsFalse)
	return up, down, rolledBack, downErr
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
	schemas := make([]string, 0, len(topicMigrationSteps))
	for _, step := range topicMigrationSteps {
		schemas = append(schemas, step.schema)
	}

	up, down, rolledBack, downErr := topicMigrations(c, realm, schemas)

	for _, step := range topicMigrationSteps {
		c.Assert(up, qt.Contains, step.up)
	}
	for _, statement := range topicMigrationDowns {
		c.Assert(down, qt.Contains, statement)
	}
	c.Assert(downErr, qt.IsNil, qt.Commentf("migrations down:\n%s", rolledBack))
}

// consumerOrderSchemas declare a topic's consumers out of name order, then
// keep one, drop two and add two more, then drop the topic.
var consumerOrderSchemas = []string{
	"topics:\n  events:\n    min_active_partitions: 1\n    consumers:\n      zz: {}\n      aa: {}\n      mm: {}\n",
	"topics:\n  events:\n    min_active_partitions: 1\n    consumers:\n      zz: {}\n      qq: {}\n      bb: {}\n",
	"topics: {}\n",
}

// The up and down files of consumerOrderSchemas, as the host's embedded
// engine wrote them, recorded from 74de0b65b on 25.1.4.7 and 26.2.1.14. A
// declared topic lists its consumers in the order the declaration does, and a
// read topic in the order YDB describes them, the order they were added in:
// the rollback of the drop creates zz, qq and bb in that order.
var (
	consumerOrderUps = []string{
		"CREATE TOPIC `events` (CONSUMER `zz`, CONSUMER `aa`, CONSUMER `mm`) WITH (min_active_partitions = 1);",
		"ALTER TOPIC `events` DROP CONSUMER `aa`, DROP CONSUMER `mm`, ADD CONSUMER `qq`, ADD CONSUMER `bb`;",
		"DROP TOPIC `events`;",
	}
	consumerOrderDowns = []string{
		"-- Recovery limit: \"consumer \\\"aa\\\" of topic events was dropped; the rollback adds it again without its position\"\n" +
			"-- Recovery limit: \"consumer \\\"mm\\\" of topic events was dropped; the rollback adds it again without its position\"\n" +
			"-- Recovery limit: \"consumer \\\"qq\\\" the change added to topic events is dropped by the rollback, with its position\"\n" +
			"-- Recovery limit: \"consumer \\\"bb\\\" the change added to topic events is dropped by the rollback, with its position\"\n" +
			"ALTER TOPIC `events` DROP CONSUMER `qq`, DROP CONSUMER `bb`, ADD CONSUMER `aa`, ADD CONSUMER `mm`;",
		"CREATE TOPIC `events` (CONSUMER `zz`, CONSUMER `qq`, CONSUMER `bb`) WITH (min_active_partitions = 1, auto_partitioning_strategy = 'disabled', " +
			"retention_period = Interval('P1D'), partition_write_speed_bytes_per_second = 1048576, partition_write_burst_bytes = 1048576);",
	}
)

// A topic keeps its consumers in their order through this provider, as the
// host's embedded engine kept them: the declaration's order in what a
// migration creates and adds, and YDB's order in what undoes a change or a
// drop. The provider's models kept consumers in name order, so every file
// listed them by name, the rollback of a drop among them.
func TestHostKeepsATopicsConsumersInTheirOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)

	up, down, rolledBack, downErr := topicMigrations(c, realm, consumerOrderSchemas)

	for _, statement := range consumerOrderUps {
		c.Assert(up, qt.Contains, statement)
	}
	for _, statement := range consumerOrderDowns {
		c.Assert(down, qt.Contains, statement)
	}
	c.Assert(downErr, qt.IsNil, qt.Commentf("migrations down:\n%s", rolledBack))
}

// `schema inspect` writes a topic's consumers in the order YDB describes them,
// as the built-in engine at host 74de0b65b wrote them on 25.1.4.7 and
// 26.2.1.14, where the provider wrote them in name order.
func TestHostInspectsATopicsConsumersInYDBsOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	topic := path.Join(database, directory, "t")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TOPIC `"+topic+"` (CONSUMER zz, CONSUMER aa, CONSUMER mm)"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TOPIC `"+topic+"`"), qt.IsNil)
	})

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE TOPIC `"+directory+"/t` (CONSUMER `zz`, CONSUMER `aa`, CONSUMER `mm`)")
}
