//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The tests in this file run the host's versioned migration verbs through this
// provider, as the host's tests run them through its built-in YDB engine:
// `migrations up` splits each body with the provider's SplitMigration, runs a
// scheme query on its own, a run of data statements in one transaction with
// its checkpoint, and records the revision through the history service.

const (
	applied = protocol.HistoryRevision_APPLIED
	failed  = protocol.HistoryRevision_FAILED
)

// usersMigrations is the host's CLI migration directory: the first migration
// carries a named expression across a CREATE TABLE into the INSERT after it.
func usersMigrations(c *qt.C, f migrationsFixture) (migrations, tests string) {
	c.Helper()
	root := c.TempDir()
	migrations = writeFiles(c, filepath.Join(root, "migrations"), map[string]string{
		"0000000001_users.up.sql": "CREATE TABLE `" + f.table("users") + "` (id Int64 NOT NULL, name Utf8, PRIMARY KEY (id));\n" +
			"$name = 'first'u;\nINSERT INTO `" + f.table("users") + "` (id, name) VALUES (1l, $name);\n",
		"0000000001_users.down.sql": "DROP TABLE `" + f.table("users") + "`;\n",
		"0000000002_posts.up.sql":   "CREATE TABLE `" + f.table("posts") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n",
		"0000000002_posts.down.sql": "DROP TABLE `" + f.table("posts") + "`;\n",
	})
	tests = writeFiles(c, filepath.Join(root, "tests"), map[string]string{"users.yaml": "cases:\n  - name: the first user exists\n    steps:\n" +
		"      - migrate_to: latest\n      - assert:\n          query: \"SELECT COUNT(*) FROM `" + f.table("users") + "`\"\n          scalar: \"1\"\n"})
	return migrations, tests
}

// The host's versioned verbs hash and validate the directory, apply it under a
// statement timeout, report it up to date, run the declarative tests and roll
// everything back.
func TestHostRunsVersionedMigrations_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	migrations, tests := usersMigrations(c, f)

	hashed, hashErr := hostOffline(c, "migrations", "hash", "--dir", migrations)
	validated, validateErr := hostOffline(c, "migrations", "validate", "--dir", migrations)
	up, upErr := hostCommand(c, append([]string{"migrations", "up", "--statement-timeout", "30s"}, f.target(migrations)...)...)
	status, statusErr := hostCommand(c, append([]string{"migrations", "status"}, f.target(migrations)...)...)
	tested, testErr := hostCommand(c, "migrations", "test", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--dir", tests,
		"--migrations-dir", migrations, "--migrations-schema", f.dir)
	down, downErr := hostCommand(c, append([]string{"migrations", "down", "--target", "0", "--confirm"}, f.target(migrations)...)...)

	c.Assert(hashErr, qt.IsNil, qt.Commentf("hash:\n%s", hashed))
	c.Assert(validateErr, qt.IsNil, qt.Commentf("validate:\n%s", validated))
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", up))
	c.Assert(up, qt.Contains, "Database is now at version: 2")
	c.Assert(statusErr, qt.IsNil, qt.Commentf("status:\n%s", status))
	c.Assert(status, qt.Contains, "Current Version: 2")
	c.Assert(status, qt.Contains, "Pending Migrations: 0")
	c.Assert(testErr, qt.IsNil, qt.Commentf("test:\n%s", tested))
	c.Assert(tested, qt.Contains, `PASS  case "the first user exists"`)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", down))
	c.Assert(f.names(c), qt.DeepEquals, []string{"schema_migrations", "schema_migrations_log"})
}

// A lock timeout is refused before anything runs: YDB has no lock wait the host
// can bound around a migration, and the provider's session lists no
// migration_lock_timeout capability.
func TestHostRefusesAMigrationLockTimeout_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	migrations, _ := usersMigrations(c, f)

	refused, err := hostCommand(c, append([]string{"migrations", "up", "--lock-timeout", "5s"}, f.target(migrations)...)...)

	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.Contains, `--lock-timeout sets a timeout for every migration, and dialect "ydb" has `+
		`no lock wait Ptah can bound around a migration (target capability migration_lock_timeout)`)
	c.Assert(f.names(c), qt.HasLen, 0)
}

// `migrations up` waits for the migration lock another session holds until
// the holder releases, and not before. Two runs started together over the same
// directory then apply it once, which the INSERT in the first migration would
// refuse to do twice.
//
// The host's run under --migration-lock-timeout is left out: the host passes
// its deadline into each provider TryLock, so a deadline that ends while the
// call is in flight reports the lock's outcome unknown rather than the
// timeout, and which one a run reports depends on timing.
func TestHostMigrationsUpWaitsForTheLock_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	migrations, _ := usersMigrations(c, f)
	up := append([]string{"migrations", "up"}, f.target(migrations)...)
	binary, digest := providerBinary(c)
	holder, session, _ := historySession(c, binary, digest)
	lock := nativeLockRequest(session, "holder", "ptah_migrate")
	acquireNativeLock(c, holder, lock)

	host, manifest := hostManifest(c)
	var waitingOutput bytes.Buffer
	waiting := exec.CommandContext(c.Context(), host, append(up, "--provider-config", manifest, "--provider-engine", "ydb")...)
	waiting.Stdout, waiting.Stderr = &waitingOutput, &waitingOutput
	started := time.Now()
	c.Assert(waiting.Start(), qt.IsNil)
	exited := make(chan error, 1)
	go func() { exited <- waiting.Wait() }()
	time.Sleep(3 * time.Second)
	c.Assert(exited, qt.HasLen, 0, qt.Commentf("output:\n%s", &waitingOutput))
	c.Assert(f.names(c), qt.Not(qt.Contains), "users")
	_, err := holder.ReleaseLock(c.Context(), lock)
	c.Assert(err, qt.IsNil)
	c.Assert(<-exited, qt.IsNil, qt.Commentf("output:\n%s", &waitingOutput))
	c.Assert(time.Since(started) >= 3*time.Second, qt.IsTrue)
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("users")+"`"), qt.Equals, int64(1))

	down, downErr := hostCommand(c, append([]string{"migrations", "down", "--target", "0", "--confirm"}, f.target(migrations)...)...)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", down))
	results := make(chan string, 2)
	for range 2 {
		go func() {
			output, err := hostCommand(c, up...)
			results <- output + "\nerror: " + fmt.Sprint(err)
		}()
	}
	first, second := <-results, <-results
	c.Assert(first, qt.Contains, "\nerror: <nil>")
	c.Assert(second, qt.Contains, "\nerror: <nil>")
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("users")+"`"), qt.Equals, int64(1))
	c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", applied, 2, 2}, {"2", applied, 1, 1}})
}

// A migration runs as the queries YDB runs it as: each scheme statement a query
// of its own, each run of data statements one query, and a named expression
// carried into every query after the one that defines it. The revision counts
// those queries, in both revision table formats, and a rollback undoes the
// migration.
func TestHostRunsQueriesAndRollsBack_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		flag   string
		format protocol.HistoryScope_Format
		state  protocol.HistoryRevision_State
	}{
		{name: "ptah format", flag: "ptah", format: protocol.HistoryScope_NATIVE, state: applied},
		{name: "atlas format", flag: "atlas", format: protocol.HistoryScope_ATLAS, state: protocol.HistoryRevision_STATE_UNSPECIFIED},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			f := newMigrationsFixture(c)
			migrations := writeFiles(c, c.TempDir(), map[string]string{
				"0000000001_items.up.sql": "--!syntax_v1\n" +
					"CREATE TABLE `" + f.table("items") + "` (id Int64 NOT NULL, name Utf8, PRIMARY KEY (id));\n" +
					"$name = 'first'u;\n" +
					"UPSERT INTO `" + f.table("items") + "` (id, name) VALUES (1l, $name);\n" +
					"UPSERT INTO `" + f.table("items") + "` (id, name) VALUES (2l, $name || 'x'u);\n" +
					"ALTER TABLE `" + f.table("items") + "` ADD COLUMN note Utf8;\n" +
					"UPDATE `" + f.table("items") + "` SET note = $name WHERE id = 1l;\n",
				"0000000001_items.down.sql": "DROP TABLE `" + f.table("items") + "`;\n",
				"0000000002_tags.up.sql": "CREATE TABLE `" + f.table("tags") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
					"INSERT INTO `" + f.table("tags") + "` (id) VALUES (1l);\n",
				"0000000002_tags.down.sql": "DROP TABLE `" + f.table("tags") + "`;\n",
			})
			target := f.target(migrations, "--revision-format", test.flag)
			before := f.progress(c, test.format)

			up, upErr := hostCommand(c, append([]string{"migrations", "up"}, target...)...)
			afterUp := f.progress(c, test.format)
			items := f.count(c, "SELECT COUNT(*) FROM `"+f.table("items")+"`")
			noted := f.count(c, "SELECT COUNT(*) FROM `"+f.table("items")+"` WHERE note = 'first'u AND id = 1l")
			tables := f.names(c)
			down, downErr := hostCommand(c, append([]string{"migrations", "down", "--target", "0", "--confirm"}, target...)...)

			c.Assert(before, qt.HasLen, 0)
			c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", up))
			c.Assert(afterUp, qt.DeepEquals, []progress{{"1", test.state, 4, 4}, {"2", test.state, 2, 2}})
			c.Assert(items, qt.Equals, int64(2))
			c.Assert(noted, qt.Equals, int64(1))
			c.Assert(tables, qt.Contains, "items")
			c.Assert(tables, qt.Contains, "tags")
			c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s", down))
			c.Assert(f.progress(c, test.format), qt.HasLen, 0)
			c.Assert(f.names(c), qt.Not(qt.Contains), "items")
			c.Assert(f.names(c), qt.Not(qt.Contains), "tags")
		})
	}
}

// A migration that fails partway keeps the queries that ran before the failure,
// which YDB commits each on its own, and records how many. After the failing
// query is fixed, an --allow-dirty run resumes at that query: the INSERT before
// it, which would answer `Conflict with existing key`, is not run again.
func TestHostResumesWhereAFailedMigrationStopped_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	head := "CREATE TABLE `" + f.table("a") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
		"$id = 1l;\nINSERT INTO `" + f.table("a") + "` (id) VALUES ($id);\n"
	down := "DROP TABLE `" + f.table("b") + "`;\nDROP TABLE `" + f.table("a") + "`;\n"
	broken := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_tables.up.sql":   head + "CREATE TABLE `" + f.table("b") + "` (id NoSuchType NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_tables.down.sql": down,
	})
	fixed := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_tables.up.sql":   head + "CREATE TABLE `" + f.table("b") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_tables.down.sql": down,
	})

	failure, failErr := hostCommand(c, append([]string{"migrations", "up"}, f.target(broken)...)...)
	stopped := f.progress(c, protocol.HistoryScope_NATIVE)
	rows := f.count(c, "SELECT COUNT(*) FROM `"+f.table("a")+"`")
	resumed, resumeErr := hostCommand(c, append([]string{"migrations", "up", "--allow-dirty"}, f.target(fixed)...)...)

	c.Assert(failErr, qt.IsNotNil)
	// The built-in engine prints YDB's issue, `Unknown simple type
	// 'NoSuchType'`. The host leaves a provider's execution message out of
	// the error it prints, so the outcome and the statement show, and a host
	// of contract 25.18 prints the provider's reason between them.
	c.Assert(failure, qt.Contains, "failed to apply migration 1: failed to execute migration SQL: provider execution failed (0 of 1 units confirmed): "+
		"provider execution: NOT_APPLIED (0 units confirmed applied)")
	c.Assert(failure, qt.Contains, "\nSQL: $id = 1l;\nCREATE TABLE `"+f.table("b")+"` (id NoSuchType NOT NULL, PRIMARY KEY (id))")
	c.Assert(failure, qt.Not(qt.Contains), "Unknown simple type")
	c.Assert(stopped, qt.DeepEquals, []progress{{"1", failed, 2, 3}})
	c.Assert(rows, qt.Equals, int64(1))
	c.Assert(resumeErr, qt.IsNil, qt.Commentf("resume:\n%s", resumed))
	c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", applied, 3, 3}})
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("a")+"`"), qt.Equals, int64(1))
	c.Assert(f.names(c), qt.Contains, "b")
}

// The data statements between two scheme statements run as one transaction:
// when the second INSERT conflicts, the first is not applied either, and the
// revision says no data query ran. The fixed file resumes at that query.
func TestHostRunsADataQueryAsOneTransaction_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	create := "CREATE TABLE `" + f.table("c") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n"
	down := "DROP TABLE `" + f.table("c") + "`;\n"
	conflicting := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_rows.up.sql":   create + "INSERT INTO `" + f.table("c") + "` (id) VALUES (1l);\nINSERT INTO `" + f.table("c") + "` (id) VALUES (1l);\n",
		"0000000001_rows.down.sql": down,
	})
	fixed := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_rows.up.sql":   create + "INSERT INTO `" + f.table("c") + "` (id) VALUES (1l);\nINSERT INTO `" + f.table("c") + "` (id) VALUES (2l);\n",
		"0000000001_rows.down.sql": down,
	})

	failure, failErr := hostCommand(c, append([]string{"migrations", "up"}, f.target(conflicting)...)...)
	rows := f.count(c, "SELECT COUNT(*) FROM `"+f.table("c")+"`")
	stopped := f.progress(c, protocol.HistoryScope_NATIVE)
	resumed, resumeErr := hostCommand(c, append([]string{"migrations", "up", "--allow-dirty"}, f.target(fixed)...)...)

	c.Assert(failErr, qt.IsNotNil)
	// The built-in engine prints YDB's `Conflict with existing key`; through
	// a provider the host prints the outcome and the data query, and a host
	// of contract 25.18 prints the provider's reason between them.
	c.Assert(failure, qt.Contains, "failed to apply migration 1: failed to execute migration SQL: provider execution failed (0 of 1 units confirmed): "+
		"provider execution: NOT_APPLIED (0 units confirmed applied)")
	c.Assert(failure, qt.Contains, "\nSQL: INSERT INTO `"+f.table("c")+"` (id) VALUES (1l);\nINSERT INTO `"+f.table("c")+"` (id) VALUES (1l)")
	c.Assert(failure, qt.Not(qt.Contains), "Conflict with existing key")
	c.Assert(rows, qt.Equals, int64(0))
	c.Assert(stopped, qt.DeepEquals, []progress{{"1", failed, 1, 2}})
	c.Assert(resumeErr, qt.IsNil, qt.Commentf("resume:\n%s", resumed))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("c")+"`"), qt.Equals, int64(2))
}

// delimiterReason is what the provider says of a client delimiter directive.
const delimiterReason = "a delimiter directive has no meaning in YQL, which ends every statement with a semicolon; " +
	"remove the directive and terminate each statement with a semicolon"

// A body that cannot be split the way YDB reads it is refused before any of its
// statements runs and before its revision is written. The words are the
// provider's, which name the refused text as the built-in engine's do.
func TestHostRefusesWhatItCannotSplit_FailurePath(t *testing.T) {
	// {t} stands for the table the body creates, which is the fixture's.
	const create = "CREATE TABLE `{t}` (id Int64 NOT NULL, PRIMARY KEY (id));\n"
	for _, test := range []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "a client delimiter", body: "DELIMITER //\n" + create,
			wantErr: `migration 1 cannot run up on ydb: the provider refuses "DELIMITER //": client delimiter directive in YQL: ` + delimiterReason},
		{name: "an Atlas delimiter", body: "-- atlas:delimiter //\n" + create,
			wantErr: `migration 1 cannot run up on ydb: the provider refuses "-- atlas:delimiter //": client delimiter directive in YQL: ` + delimiterReason},
		{name: "the ANSI lexer", body: "--!ansi_lexer\n" + create,
			wantErr: `migration 1 cannot run up on ydb: the provider refuses "--!ansi_lexer": unsupported YQL translation setting: ` +
				`YQL is split as --!syntax_v1 reads it, and YDB would read this text differently or refuse it; remove the line`},
		{name: "a commit", body: create + "INSERT INTO `{t}` (id) VALUES (1l);\nCOMMIT;\n",
			wantErr: `migration 1 cannot run up on ydb: the provider refuses "COMMIT;": the statement controls a transaction, ` +
				`and the host runs each data query in a transaction of its own; remove it`},
		// YDB refuses the block whole, with `Queries with mixed data and scheme
		// operations are not supported`; the CREATE TABLE before it is a query
		// of its own, which would stay applied if the block ran.
		{name: "a block that writes and creates", body: create +
			"DO BEGIN UPSERT INTO `{t}` (id) VALUES (1l); CREATE TABLE `{t}_m` (id Int64 NOT NULL, PRIMARY KEY (id)); END DO;\n",
			wantErr: "migration 1 cannot run up on ydb: the provider refuses \"DO BEGIN UPSERT INTO `{t}` (id) VALUES (1l); CREATE TABLE `{t}_m` " +
				"(id Int64 NOT NULL, PRIMARY KEY (id)); END DO;\": the statement runs a scheme statement and a statement that reads or writes a table in one query, " +
				"which YDB refuses whole (Queries with mixed data and scheme operations are not supported); move the scheme statement out of the block or action into a statement of its own"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			f := newMigrationsFixture(c)
			migrations := writeFiles(c, c.TempDir(), map[string]string{
				"0000000001_r.up.sql":   strings.ReplaceAll(test.body, "{t}", f.table("r")),
				"0000000001_r.down.sql": "DROP TABLE `" + f.table("r") + "`;\n",
			})

			refused, err := hostCommand(c, append([]string{"migrations", "up"}, f.target(migrations)...)...)

			c.Assert(err, qt.IsNotNil)
			c.Assert(refused, qt.Contains, strings.ReplaceAll(test.wantErr, "{t}", f.table("r")))
			c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.HasLen, 0)
			c.Assert(f.names(c), qt.Not(qt.Contains), "r")
		})
	}
}

// A tag is recorded, moved, resolved and deleted, a second delete is refused,
// and the log records the run's one attempt. Both tables live in the migrations
// directory.
func TestHostRecordsTagsAndTheLog_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_t.up.sql":   "CREATE TABLE `" + f.table("t") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_t.down.sql": "DROP TABLE `" + f.table("t") + "`;\n",
	})
	tag := func(args ...string) (string, error) {
		return hostCommand(c, append(append([]string{"migrations", "tag"}, args...), "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--migrations-schema", f.dir)...)
	}
	up, upErr := hostCommand(c, append([]string{"migrations", "up"}, f.target(migrations)...)...)
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", up))

	recorded, recordErr := tag("release", "--version", "0")
	moved, moveErr := tag("release", "--version", "1")
	listed, listErr := tag()
	deleted, deleteErr := tag("release", "--delete")
	again, againErr := tag("release", "--delete")
	logged, logErr := hostCommand(c, "migrations", "log", "--json", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--migrations-schema", f.dir)

	c.Assert(recordErr, qt.IsNil, qt.Commentf("%s", recorded))
	c.Assert(recorded, qt.Contains, "Tagged version 0 as release.")
	c.Assert(moveErr, qt.IsNil, qt.Commentf("%s", moved))
	c.Assert(moved, qt.Contains, "Tagged version 1 as release.")
	c.Assert(listErr, qt.IsNil, qt.Commentf("%s", listed))
	c.Assert(listed, qt.Matches, `(?s)TAG\s+VERSION\s+RECORDED\nrelease\s+1\s+\S+\n`)
	c.Assert(deleteErr, qt.IsNil, qt.Commentf("%s", deleted))
	c.Assert(deleted, qt.Contains, "Removed tag release.")
	c.Assert(againErr, qt.IsNotNil)
	c.Assert(again, qt.Contains, `error: migration tag not found: "release"`)
	c.Assert(logErr, qt.IsNil, qt.Commentf("%s", logged))
	var attempts []struct {
		Operation string `json:"operation"`
		Version   int64  `json:"version"`
		Outcome   string `json:"outcome"`
	}
	c.Assert(json.Unmarshal([]byte(logged), &attempts), qt.IsNil, qt.Commentf("%s", logged))
	c.Assert(attempts, qt.HasLen, 1)
	c.Assert(attempts[0].Operation, qt.Equals, "up")
	c.Assert(attempts[0].Version, qt.Equals, int64(1))
	c.Assert(attempts[0].Outcome, qt.Equals, "applied")
	c.Assert(f.names(c), qt.DeepEquals, []string{"ptah_migration_tags", "schema_migrations", "schema_migrations_log", "t"})
}

// A resume skips only a prefix it can prove unchanged: a file whose committed
// INSERT was edited is refused rather than resumed past the edit.
func TestHostRefusesToResumeOverAnEditedPrefix_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	// The edit is in the INSERT, the second statement of the second query, so a
	// digest over statements rather than queries would stop short of it.
	body := func(value, fType string) map[string]string {
		return map[string]string{
			"0000000001_e.up.sql": "CREATE TABLE `" + f.table("e") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
				"$id = 1l;\nINSERT INTO `" + f.table("e") + "` (id) VALUES (" + value + ");\n" +
				"CREATE TABLE `" + f.table("f") + "` (id " + fType + " NOT NULL, PRIMARY KEY (id));\n",
			"0000000001_e.down.sql": "DROP TABLE `" + f.table("f") + "`;\nDROP TABLE `" + f.table("e") + "`;\n",
		}
	}
	failure, failErr := hostCommand(c, append([]string{"migrations", "up"}, f.target(writeFiles(c, c.TempDir(), body("$id", "NoSuchType")))...)...)
	c.Assert(failErr, qt.IsNotNil, qt.Commentf("%s", failure))

	refused, err := hostCommand(c, append([]string{"migrations", "up", "--allow-dirty"}, f.target(writeFiles(c, c.TempDir(), body("$id * 7l", "Int64")))...)...)

	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.Contains, `migration 1 cannot resume automatically: the already committed statement prefix `+
		`changed after 2 of 3 statements committed; inspect the database before choosing a repair point`)
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("e")+"` WHERE id = 7l"), qt.Equals, int64(0))
	c.Assert(f.names(c), qt.Not(qt.Contains), "f")
}

// The migrations directory need not exist: a revision table there is absent
// until the first run creates the directory along with it, and asking about it
// creates nothing.
func TestHostAsksAboutADirectoryThatDoesNotExist_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_a.up.sql":   "CREATE TABLE `" + f.table("a") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_a.down.sql": "DROP TABLE `" + f.table("a") + "`;\n",
	})

	status, err := hostCommand(c, append([]string{"migrations", "status"}, f.target(migrations)...)...)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", status))
	c.Assert(status, qt.Contains, "Pending Migrations: 1")
	c.Assert(heldEntries(c, f.opened, f.database+"/"+f.dir), qt.IsNil)
}

// A repair that resumes the body runs the rest of it from the statement named:
// the INSERT runs once, and the revision is applied.
func TestHostRepairResumesTheBody_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	head := "CREATE TABLE `" + f.table("r") + "` (id Int64 NOT NULL, note Utf8, PRIMARY KEY (id));\n"
	down := "DROP TABLE `" + f.table("r") + "`;\n"
	broken := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_r.up.sql":   head + "CREATE TABLE `" + f.table("bad") + "` (id NoSuchType NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_r.down.sql": down,
	})
	fixed := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_r.up.sql":   head + "INSERT INTO `" + f.table("r") + "` (id, note) VALUES (1l, 'repaired'u);\n",
		"0000000001_r.down.sql": down,
	})
	failure, failErr := hostCommand(c, append([]string{"migrations", "up"}, f.target(broken)...)...)
	c.Assert(failErr, qt.IsNotNil, qt.Commentf("%s", failure))
	failedStatement := f.failedStatement(c, "1")

	repaired, err := hostCommand(c, append([]string{"migrations", "repair", "--version", "1", "--resume-from", "2"}, f.target(fixed)...)...)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", repaired))
	c.Assert(failedStatement, qt.Equals, "CREATE TABLE `"+f.table("bad")+"` (id NoSuchType NOT NULL, PRIMARY KEY (id))")
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("r")+"` WHERE note = 'repaired'u"), qt.Equals, int64(1))
	c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", applied, 2, 2}})
}

// A repair that resumes the body refuses what a run refuses, before any of the
// resumed statements runs: the CREATE TABLE after the client delimiter is not
// run.
func TestHostRepairRefusesWhatItCannotSplit_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	head := "CREATE TABLE `" + f.table("r") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n"
	down := "DROP TABLE `" + f.table("r") + "`;\n"
	broken := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_r.up.sql":   head + "CREATE TABLE `" + f.table("bad") + "` (id NoSuchType NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_r.down.sql": down,
	})
	delimited := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_r.up.sql":   head + "DELIMITER //\nCREATE TABLE `" + f.table("s") + "` (id Int64 NOT NULL, PRIMARY KEY (id))//\n",
		"0000000001_r.down.sql": down,
	})
	failure, failErr := hostCommand(c, append([]string{"migrations", "up"}, f.target(broken)...)...)
	c.Assert(failErr, qt.IsNotNil, qt.Commentf("%s", failure))

	refused, err := hostCommand(c, append([]string{"migrations", "repair", "--version", "1", "--resume-from", "2"}, f.target(delimited)...)...)

	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.Contains, `migration 1 cannot run up on ydb: the provider refuses "DELIMITER //": client delimiter directive in YQL: `+delimiterReason)
	c.Assert(f.names(c), qt.Not(qt.Contains), "s")
	c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", failed, 1, 2}})
}

// A block or an action that runs a scheme statement, and a BATCH statement, run
// outside a transaction, as YDB requires of each: inside the transaction a data
// query runs in, YDB refuses the first with `Scheme operations cannot be
// executed inside transaction` and the second with `BATCH operation can be
// executed only in the implicit transaction mode`.
func TestHostRunsBlocksAndBatchStatementsOutsideATransaction_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_blocks.up.sql": "DO BEGIN\n  CREATE TABLE `" + f.table("a") + "` (id Int64 NOT NULL, n Int64, PRIMARY KEY (id));\nEND DO;\n" +
			"DEFINE ACTION $make($name) AS\n  CREATE TABLE $name (id Int64 NOT NULL, PRIMARY KEY (id));\nEND DEFINE;\n" +
			"EVALUATE FOR $table IN AsList('" + f.table("b") + "', '" + f.table("c") + "') DO $make($table);\n" +
			"UPSERT INTO `" + f.table("a") + "` (id, n) VALUES (1l, 1l), (2l, 2l), (3l, 3l);\n" +
			"BATCH DELETE FROM `" + f.table("a") + "` WHERE id = 2l;\n" +
			"BATCH UPDATE `" + f.table("a") + "` SET n = 0l WHERE id > 0l;\n" +
			"DO $make('" + f.table("d") + "');\n",
		"0000000001_blocks.down.sql": "DROP TABLE `" + f.table("d") + "`;\nDROP TABLE `" + f.table("c") + "`;\nDROP TABLE `" + f.table("b") +
			"`;\nDROP TABLE `" + f.table("a") + "`;\n",
	})

	up, err := hostCommand(c, append([]string{"migrations", "up"}, f.target(migrations)...)...)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", up))
	c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", applied, 6, 6}})
	c.Assert(f.names(c), qt.DeepEquals, []string{"a", "b", "c", "d", "schema_migrations", "schema_migrations_log"})
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("a")+"` WHERE n = 0l"), qt.Equals, int64(2))
}

// A dry run executes nothing, a data query included.
func TestHostDryRunRunsNoDataQuery_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	c.Assert(execFixtureSchema(c.Context(), f.opened.DB, "CREATE TABLE `"+f.database+"/"+f.table("q")+"` (id Int64 NOT NULL, PRIMARY KEY (id))"), qt.IsNil)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_q.up.sql": "INSERT INTO `" + f.table("q") + "` (id) VALUES (1l);\n" +
			"CREATE TABLE `" + f.table("r") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_q.down.sql": "DROP TABLE `" + f.table("r") + "`;\n",
	})

	dry, err := hostCommand(c, append([]string{"migrations", "up", "--dry-run"}, f.target(migrations)...)...)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", dry))
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("q")+"`"), qt.Equals, int64(0))
	c.Assert(f.names(c), qt.DeepEquals, []string{"q"})
}

// gateMigrations writes a directory whose second version adds a unique index to
// the table its first version created, under the lint policy given.
func gateMigrations(c *qt.C, f migrationsFixture, policy string) string {
	c.Helper()
	users := "`" + f.table("users") + "`"
	return writeFiles(c, c.TempDir(), map[string]string{
		".ptah-lint.yaml":            policy,
		"0000000001_users.up.sql":    "CREATE TABLE " + users + " (id Uint64 NOT NULL, email Utf8, PRIMARY KEY (id));\n",
		"0000000001_users.down.sql":  "DROP TABLE " + users + ";\n",
		"0000000002_unique.up.sql":   "ALTER TABLE " + users + " ADD INDEX users_email GLOBAL UNIQUE SYNC ON (email);\n",
		"0000000002_unique.down.sql": "ALTER TABLE " + users + " DROP INDEX users_email;\n",
	})
}

// withoutUniqueIndexOnExistingTables turns EnableAddUniqueIndex off until the
// test ends on a server whose session establishes
// unique_index_on_existing_table, as the 26.2 test server's does, so that a
// unique index added to a table that exists is refused there. A server whose
// session lacks the capability is left as it is.
func withoutUniqueIndexOnExistingTables(c *qt.C, opened *connection.Connection) {
	c.Helper()
	if !targetCapabilities(c)[tablecreate.UniqueIndexOnExistingTableCapability] {
		return
	}
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableAddUniqueIndex": false})
}

// Ported from the host's TestYDBBinary_MigrationsUpGatesOnTheYDFamily:
// `migrations up` lints the pending migrations through this provider before it
// runs any of them. A policy whose gate section blocks on the YD family refuses
// the unique index the second version adds to the table the first version
// creates, in YD101's words, and no migration runs: the directory holds the
// revision table the host creates first, and no users. Without the gate section
// the first version runs and the server refuses the second. The host's test
// quotes the server's words there; the provider keeps YDB's issue text out of
// the reason it gives (see package ydbfailure), so the refusal is the
// provider's: on 26.2 the capability and the flag YDB refused for, and on 25.1
// the status and issue code of an answer that does not show whether the
// statement applied. The 26.2 test server starts with EnableAddUniqueIndex on,
// under which it builds the index, so the test turns the flag off there while
// it runs.
func TestHostMigrationsUpGatesOnTheYDFamily_FailurePath(t *testing.T) {
	refusals := map[string]string{
		"26.2": `capability unique_index_on_existing_table is off on this YDB cluster, which runs with feature flag EnableAddUniqueIndex off: ` +
			`YDB refused the statement \(BAD_REQUEST\)`,
		"25.1": `the unit's outcome is unknown: YDB answered GENERIC_ERROR, issue 1060, line 1, column \d+, which does not show whether the unit applied`,
	}
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	withoutUniqueIndexOnExistingTables(c, f.opened)
	refusal, measured := refusals[serverLine(c, serverVersion(c, f.opened))]
	c.Assert(measured, qt.IsTrue)

	gated, gatedErr := hostCommand(c, append([]string{"migrations", "up"},
		f.target(gateMigrations(c, f, "dialect: ydb\ngate:\n  families: [YD]\n"))...)...)
	namesAfterGate := f.names(c)
	ungated, ungatedErr := hostCommand(c, append([]string{"migrations", "up"}, f.target(gateMigrations(c, f, "dialect: ydb\n"))...)...)

	c.Assert(gatedErr, qt.IsNotNil)
	c.Assert(gated, qt.Matches, `(?s).*pending migrations carry lint findings the policy's gate section blocks on.*`+
		`0000000002_unique\.up\.sql:1 YD101 error: ADD INDEX users_email adds a unique index to `+regexp.QuoteMeta(f.table("users"))+`.*`)
	c.Assert(namesAfterGate, qt.Not(qt.Contains), "users")
	c.Assert(ungatedErr, qt.IsNotNil)
	c.Assert(ungated, qt.Matches, `(?s).*failed to apply migration 2: .*`+refusal+`.*`)
	c.Assert(f.names(c), qt.Contains, "users")
}
