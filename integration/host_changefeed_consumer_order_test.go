//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// changefeedConsumerSchemas declare a new table's changefeed with its topic's
// consumers out of name order, then keep one, drop two and add two more, then
// drop the changefeed.
var changefeedConsumerSchemas = []string{
	"tables:\n  t:\n    columns:\n      id: {type: Uint64, primary: true}\n    changefeeds:\n      feed:\n        mode: UPDATES\n        format: JSON\n" +
		"        consumers:\n          zz: {}\n          aa: {}\n          mm: {}\n",
	"tables:\n  t:\n    columns:\n      id: {type: Uint64, primary: true}\n    changefeeds:\n      feed:\n        mode: UPDATES\n        format: JSON\n" +
		"        consumers:\n          zz: {}\n          qq: {}\n          bb: {}\n",
	"tables:\n  t:\n    columns:\n      id: {type: Uint64, primary: true}\n",
}

// The statements of changefeedConsumerSchemas' up and down files, as the
// host's embedded engine at 74de0b65b wrote them on 25.1.4.7 and 26.2.1.14: a
// declared changefeed's consumers in the declaration's order, a read one's in
// name order. The rollback of the change drops the consumers it added in the
// order it added them, and the rollback of the drop adds bb, qq and zz.
var (
	changefeedConsumerUps = []string{
		"ALTER TABLE `t` ADD CHANGEFEED `feed` WITH (MODE = 'UPDATES', FORMAT = 'JSON');\n" +
			"ALTER TOPIC `t/feed` ADD CONSUMER `zz`;\nALTER TOPIC `t/feed` ADD CONSUMER `aa`;\nALTER TOPIC `t/feed` ADD CONSUMER `mm`;",
		"ALTER TOPIC `t/feed` DROP CONSUMER `aa`;\nALTER TOPIC `t/feed` DROP CONSUMER `mm`;\n" +
			"ALTER TOPIC `t/feed` ADD CONSUMER `qq`;\nALTER TOPIC `t/feed` ADD CONSUMER `bb`;",
	}
	changefeedConsumerDowns = []string{
		"ALTER TOPIC `t/feed` DROP CONSUMER `qq`;\nALTER TOPIC `t/feed` DROP CONSUMER `bb`;\n" +
			"ALTER TOPIC `t/feed` ADD CONSUMER `aa`;\nALTER TOPIC `t/feed` ADD CONSUMER `mm`;",
		"ALTER TABLE `t` ADD CHANGEFEED `feed` WITH (MODE = 'UPDATES', FORMAT = 'JSON');\n" +
			"ALTER TOPIC `t/feed` ADD CONSUMER `bb`;\nALTER TOPIC `t/feed` ADD CONSUMER `qq`;\nALTER TOPIC `t/feed` ADD CONSUMER `zz`;",
	}
)

// heldTopicConsumers are the consumers of the topic at absolute in the order YDB
// describes them, which is the order they were added in.
func heldTopicConsumers(c *qt.C, realm cliRealm, absolute string) []string {
	c.Helper()
	description, err := realm.opened.Driver.Topic().Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	var names []string
	for _, consumer := range description.Consumers {
		names = append(names, consumer.Name)
	}
	return names
}

// A changefeed's consumers reach YDB in the declaration's order through this
// provider, as the host's embedded engine sent them: YDB describes the topic's
// consumers in the order they were added, zz, aa and mm after the creation
// and zz, qq and bb after the change, and the up and down files hold the
// engine's statements. The provider's changefeed model kept consumers in name
// order, so it added aa, mm and zz, then bb and qq, and its rollback of the
// change dropped bb before qq.
func TestHostKeepsAChangefeedsConsumersInTheirOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	migrations := c.TempDir()
	topic := path.Join(realm.root, "t", "feed")
	step := func(i int) {
		file := filepath.Join(writeFiles(c, c.TempDir(), map[string]string{"schema.yaml": changefeedConsumerSchemas[i]}), "schema.yaml")
		generated, err := hostCommand(c, "migrations", "generate", "--db-url", realm.address, "--schema-file", file,
			"--migrations-dir", migrations, "--name", "feed")
		c.Assert(err, qt.IsNil, qt.Commentf("step %d, migrations generate:\n%s", i+1, generated))
		applied, err := hostCommand(c, "migrations", "up", "--db-url", realm.address, "--migrations-dir", migrations, "--allow-destructive")
		c.Assert(err, qt.IsNil, qt.Commentf("step %d, migrations up:\n%s", i+1, applied))
	}
	step(0)
	created := heldTopicConsumers(c, realm, topic)
	step(1)
	changed := heldTopicConsumers(c, realm, topic)
	step(2)
	up, down := generatedPair(c, migrations)

	rolledBack, err := hostCommand(c, "migrations", "down", "--db-url", realm.address, "--migrations-dir", migrations, "--target", "0", "--confirm")

	c.Assert(err, qt.IsNil, qt.Commentf("migrations down:\n%s", rolledBack))
	c.Assert(created, qt.DeepEquals, []string{"zz", "aa", "mm"})
	c.Assert(changed, qt.DeepEquals, []string{"zz", "qq", "bb"})
	for _, statements := range changefeedConsumerUps {
		c.Assert(up, qt.Contains, statements)
	}
	for _, statements := range changefeedConsumerDowns {
		c.Assert(down, qt.Contains, statements)
	}
}

// `schema inspect` writes a changefeed's consumers in name order, as the
// built-in engine at host 74de0b65b wrote them on 25.1.4.7 and 26.2.1.14,
// whatever order YDB describes them in.
func TestHostInspectsAChangefeedsConsumersByName_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(database, directory, "t")
	createSchemaObject(c, opened, "CREATE TABLE `"+table+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE `"+table+"`")
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	for _, statement := range []string{
		"ALTER TABLE `" + table + "` ADD CHANGEFEED feed WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
		"ALTER TOPIC `" + table + "/feed` ADD CONSUMER zz, ADD CONSUMER aa, ADD CONSUMER mm",
	} {
		c.Assert(execFixtureSchema(ctx, opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	feed := "ALTER TOPIC `" + directory + "/t/feed` ADD CONSUMER "
	c.Assert(output, qt.Contains, strings.Join([]string{feed + "`aa`;", feed + "`mm`;", feed + "`zz`;"}, "\n"), qt.Commentf("%s", output))
}
