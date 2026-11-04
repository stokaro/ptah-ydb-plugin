//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// The host's dev database verbs with the test database as the dev URL: each
// run gets a dev realm of its own in that database, so the replay runs
// against an empty database, nothing it creates reaches the database's own
// schema, what the database holds stays, and the realm is gone when the host
// exits. The database holds a table in a directory of its own, so it is not
// empty: a dev database that were the database itself would be refused as not
// clean, or emptied.
func TestHostDevDatabaseIsARealm_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+path.Join(database, directory, "t")+"` (`id` Int64 NOT NULL, PRIMARY KEY (`id`))"), qt.IsNil)
	c.Cleanup(func() {
		_ = execFixtureSchema(context.Background(), opened.DB, "DROP TABLE `"+path.Join(database, directory, "t")+"`")
	})
	address := os.Getenv("PTAH_YDB_TEST_URL")
	for _, test := range []struct {
		name  string
		files map[string]string
		// command runs on the migration directory, which it is given last.
		command []string
		want    string
		// absent is a name of the database root the replay creates in its realm.
		absent string
	}{
		{name: "a replay runs in a realm", files: map[string]string{
			"0000000001_users.up.sql":   "CREATE TABLE `users` (`id` Int64 NOT NULL, `name` Utf8, PRIMARY KEY (`id`));\n",
			"0000000001_users.down.sql": "DROP TABLE `users`;\n",
		}, command: []string{"migrations", "validate", "--dev-url", address, "--dir"}, want: "OK: migration SQL validated on dev database", absent: "users"},
		{name: "a topic replays in a realm and goes with it", files: map[string]string{
			"0000000001_events.up.sql":   "CREATE TOPIC `ptah_ydb_devrealm_events` (CONSUMER `reader`);\n",
			"0000000001_events.down.sql": "DROP TOPIC `ptah_ydb_devrealm_events`;\n",
			"0000000002_audit.up.sql":    "ALTER TOPIC `ptah_ydb_devrealm_events` ADD CONSUMER `audit`;\n",
			"0000000002_audit.down.sql":  "ALTER TOPIC `ptah_ydb_devrealm_events` DROP CONSUMER `audit`;\n",
		}, command: []string{"migrations", "validate", "--dev-url", address, "--dir"}, want: "OK: migration SQL validated on dev database", absent: "ptah_ydb_devrealm_events"},
		{name: "lint replays in a realm and reads its baseline there", files: map[string]string{
			"0000000001_users.up.sql":   "CREATE TABLE `shop/users` (`id` Uint64 NOT NULL, PRIMARY KEY (`id`));\n",
			"0000000001_users.down.sql": "DROP TABLE `shop/users`;\n",
			"0000000002_score.up.sql":   "ALTER TABLE `shop/users` ADD COLUMN `score` Int32;\n",
			"0000000002_score.down.sql": "ALTER TABLE `shop/users` DROP COLUMN `score`;\n",
		}, command: []string{"migrations", "lint", "--dev-url", address, "--fail-on", "none", "--dir"}, want: "No lint findings.", absent: "shop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			migrations := writeFiles(c, c.TempDir(), test.files)
			hashed, hashErr := hostOffline(c, "migrations", "hash", "--dir", migrations)
			c.Assert(hashErr, qt.IsNil, qt.Commentf("%s", hashed))

			replayed, err := hostCommand(c, append(test.command, migrations)...)

			c.Assert(err, qt.IsNil, qt.Commentf("%s", replayed))
			c.Assert(replayed, qt.Contains, test.want)
			c.Assert(replayed, qt.Not(qt.Contains), "ran without the baseline schema")
			c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), test.absent)
			c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), connection.RealmDirectory)
		})
	}
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"t": Ydb_Scheme.Entry_TABLE})
}

// A statement of a replay that reaches the whole database is refused as one its
// realm cannot confine, and the user it would create is not created.
func TestHostDevDatabaseRefusesAUserOfTheWholeDatabase_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, _, database := lockFixture(c)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_reader.up.sql":   "CREATE USER ptahydbdevrealmreader PASSWORD 'secret';\n",
		"0000000001_reader.down.sql": "DROP USER ptahydbdevrealmreader;\n",
	})
	hashed, hashErr := hostOffline(c, "migrations", "hash", "--dir", migrations)
	c.Assert(hashErr, qt.IsNil, qt.Commentf("%s", hashed))

	refused, err := hostCommand(c, "migrations", "validate", "--dir", migrations, "--dev-url", os.Getenv("PTAH_YDB_TEST_URL"))
	var users uint64
	countErr := opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `.sys/auth_users` WHERE Sid = 'ptahydbdevrealmreader'").Scan(&users)

	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.Contains, "ydb migration replay rejects a user of the whole database because its "+
		"effects cannot be confined to the disposable database realm")
	c.Assert(countErr, qt.IsNil)
	c.Assert(users, qt.Equals, uint64(0))
	c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), connection.RealmDirectory)
}
