//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The tests in this file run the host's repeatable migrations through this
// provider's record of them. The directory's versioned migration creates the
// tables; the repeatable migration R__user_names.sql inserts a row of runs
// named after its content. An INSERT that ran twice fails on the row its first
// run wrote, so a run that succeeds says the body did not run again, and the
// rows are read back from the database.

// repeatableBody is the repeatable migration that records run tag.
func repeatableBody(f migrationsFixture, tag string) string {
	return "INSERT INTO `" + f.table("runs") + "` (tag) VALUES ('" + tag + "'u);\n"
}

// repeatableMigrations is a directory with the versioned migration and the
// repeatable one at its first content.
func repeatableMigrations(c *qt.C, f migrationsFixture) string {
	c.Helper()
	return writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_tables.up.sql": "CREATE TABLE `" + f.table("users") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
			"CREATE TABLE `" + f.table("runs") + "` (tag Utf8 NOT NULL, PRIMARY KEY (tag));\n",
		"0000000001_tables.down.sql": "DROP TABLE `" + f.table("runs") + "`;\nDROP TABLE `" + f.table("users") + "`;\n",
		"R__user_names.sql":          repeatableBody(f, "v1"),
	})
}

// repeatableRun is what a test reads off the run document `migrations up
// --json` prints.
type repeatableRun struct {
	Outcome            string              `json:"outcome"`
	Applied            []int64             `json:"applied"`
	PlannedRepeatables []plannedRepeatable `json:"planned_repeatables"`
	AppliedRepeatables []string            `json:"applied_repeatables"`
	MigrationLog       struct {
		Lost []struct {
			Operation  string `json:"operation"`
			Repeatable string `json:"repeatable"`
		} `json:"lost"`
	} `json:"migration_log"`
}

// lost names each log entry the run could not write by its operation and the
// repeatable migration it is about.
func (r repeatableRun) lost() []string {
	described := make([]string, 0, len(r.MigrationLog.Lost))
	for _, entry := range r.MigrationLog.Lost {
		described = append(described, entry.Operation+" "+entry.Repeatable)
	}
	return described
}

type plannedRepeatable struct {
	Identity string `json:"identity"`
	Reason   string `json:"reason"`
}

// repeatableStatus is the record `migrations status --json` prints.
type repeatableStatus struct {
	Repeatables []struct {
		Identity        string `json:"identity"`
		State           string `json:"state"`
		AppliedChecksum string `json:"applied_checksum"`
	} `json:"repeatables"`
}

// hostDocument runs the host with args against this provider and decodes the
// JSON it prints on stdout into document. It returns what the host wrote on
// stderr and how it exited.
func hostDocument(c *qt.C, document any, args ...string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()
	command := testCommand(ctx, host, append(args, "--provider-config", manifest, "--provider-engine", "ydb")...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	c.Assert(json.Unmarshal(stdout.Bytes(), document), qt.IsNil, qt.Commentf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String()))
	return stderr.String(), err
}

// up runs `migrations up --json` against the fixture.
func (f migrationsFixture) up(c *qt.C, migrations string, extra ...string) (repeatableRun, string, error) {
	c.Helper()
	var run repeatableRun
	stderr, err := hostDocument(c, &run, append([]string{"migrations", "up", "--json"}, f.target(migrations, extra...)...)...)
	return run, stderr, err
}

// runs lists the tags the repeatable migration recorded, in order.
func (f migrationsFixture) runs(c *qt.C) []string {
	c.Helper()
	// #nosec G202 -- a fixture table the test names under its own database
	rows, err := f.opened.DB.QueryContext(c.Context(), "SELECT tag FROM `"+path.Join(f.database, f.table("runs"))+"` ORDER BY tag")
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(rows.Close(), qt.IsNil) }()
	tags := make([]string, 0)
	for rows.Next() {
		var tag string
		c.Assert(rows.Scan(&tag), qt.IsNil)
		tags = append(tags, tag)
	}
	c.Assert(rows.Err(), qt.IsNil)
	return tags
}

// attempts reads the attempts `migrations log --json` printed, newest first,
// as the version, the repeatable identity and the outcome of each.
func attempts(c *qt.C, f migrationsFixture) []string {
	c.Helper()
	var records []struct {
		Version    int64  `json:"version"`
		Repeatable string `json:"repeatable"`
		Outcome    string `json:"outcome"`
	}
	_, err := hostDocument(c, &records, "migrations", "log", "--json", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--migrations-schema", f.dir)
	c.Assert(err, qt.IsNil)
	described := make([]string, 0, len(records))
	for _, record := range records {
		described = append(described, strconv.FormatInt(record.Version, 10)+" "+record.Repeatable+" "+record.Outcome)
	}
	return described
}

// A repeatable migration applies once and an unchanged rerun runs nothing; a
// changed file runs again, the log names each attempt, lint reads the file,
// and a drift check replays the applied content on a dev realm; a row of runs
// deleted by hand comes back once repair removes the record's row.
func TestHostRunsRepeatableMigrations_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.RepeatableRecordFeature)
	f := newMigrationsFixture(c)
	migrations := repeatableMigrations(c, f)
	runsTable := path.Join(f.database, f.table("runs"))

	first, firstErr, firstExit := f.up(c, migrations)
	unchanged, unchangedErr, unchangedExit := f.up(c, migrations)
	var status repeatableStatus
	statusErr, statusExit := hostDocument(c, &status, append([]string{"migrations", "status", "--json"}, f.target(migrations)...)...)
	writeFiles(c, migrations, map[string]string{"R__user_names.sql": repeatableBody(f, "v2")})
	changed, changedErr, changedExit := f.up(c, migrations)
	logged := attempts(c, f)
	linted, lintExit := hostCommand(c, "migrations", "lint", "--dir", migrations, "--fail-on", "error")
	drift, driftErr, driftExit := f.up(c, migrations, "--check-drift", "--shadow-db", os.Getenv("PTAH_YDB_TEST_URL"))
	_, deleteErr := f.opened.DB.ExecContext(c.Context(), "DELETE FROM `"+runsTable+"` WHERE tag = 'v2'u") // #nosec G202 -- a table under a directory this test generated
	kept, keptErr, keptExit := f.up(c, migrations)
	afterDelete := f.runs(c)
	repaired, repairExit := hostCommand(c, append([]string{"migrations", "repair", "--repeatable", "user_names", "--force"}, f.target(migrations)...)...)
	rerun, rerunErr, rerunExit := f.up(c, migrations)

	c.Assert(firstExit, qt.IsNil, qt.Commentf("%s", firstErr))
	c.Assert(first.Applied, qt.DeepEquals, []int64{1})
	c.Assert(first.AppliedRepeatables, qt.DeepEquals, []string{"user_names"})
	c.Assert(unchangedExit, qt.IsNil, qt.Commentf("%s", unchangedErr))
	c.Assert(unchanged.Outcome, qt.Equals, "up-to-date")
	c.Assert(statusExit, qt.IsNil, qt.Commentf("%s", statusErr))
	c.Assert(status.Repeatables, qt.HasLen, 1)
	c.Assert(status.Repeatables[0].State, qt.Equals, "applied")
	c.Assert(changedExit, qt.IsNil, qt.Commentf("%s", changedErr))
	c.Assert(changed.PlannedRepeatables, qt.DeepEquals, []plannedRepeatable{{Identity: "user_names", Reason: "changed"}})
	c.Assert(logged, qt.DeepEquals, []string{"0 user_names applied", "0 user_names applied", "1  applied"})
	c.Assert(lintExit, qt.IsNil, qt.Commentf("%s", linted))
	c.Assert(driftExit, qt.IsNil, qt.Commentf("%s", driftErr))
	c.Assert(drift.Outcome, qt.Equals, "up-to-date")
	c.Assert(deleteErr, qt.IsNil)
	c.Assert(keptExit, qt.IsNil, qt.Commentf("%s", keptErr))
	c.Assert(kept.Outcome, qt.Equals, "up-to-date")
	c.Assert(afterDelete, qt.DeepEquals, []string{"v1"})
	c.Assert(repairExit, qt.IsNil, qt.Commentf("%s", repaired))
	c.Assert(rerunExit, qt.IsNil, qt.Commentf("%s", rerunErr))
	c.Assert(rerun.PlannedRepeatables, qt.DeepEquals, []plannedRepeatable{{Identity: "user_names", Reason: "new"}})
	c.Assert(f.runs(c), qt.DeepEquals, []string{"v1", "v2"})
}

// A body that fails leaves its row FAILED and nothing of its own; once the file
// is fixed, `migrations up --allow-dirty` retries it.
func TestHostRetriesAFailedRepeatableMigration_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.RepeatableRecordFeature)
	f := newMigrationsFixture(c)
	migrations := repeatableMigrations(c, f)
	// The second INSERT fails on the row the first wrote.
	writeFiles(c, migrations, map[string]string{"R__user_names.sql": repeatableBody(f, "broken") + repeatableBody(f, "broken")})

	_, failedErr, failedExit := f.up(c, migrations)
	var status repeatableStatus
	statusErr, statusExit := hostDocument(c, &status, append([]string{"migrations", "status", "--json"}, f.target(migrations)...)...)
	afterFailure := f.runs(c)
	writeFiles(c, migrations, map[string]string{"R__user_names.sql": repeatableBody(f, "v1")})
	retried, retriedErr, retriedExit := f.up(c, migrations, "--allow-dirty")

	c.Assert(failedExit, qt.IsNotNil, qt.Commentf("%s", failedErr))
	c.Assert(statusExit, qt.IsNil, qt.Commentf("%s", statusErr))
	c.Assert(status.Repeatables[0].State, qt.Equals, "failed")
	c.Assert(afterFailure, qt.HasLen, 0)
	c.Assert(retriedExit, qt.IsNil, qt.Commentf("%s", retriedErr))
	c.Assert(retried.PlannedRepeatables, qt.DeepEquals, []plannedRepeatable{{Identity: "user_names", Reason: "retry"}})
	c.Assert(f.runs(c), qt.DeepEquals, []string{"v1"})
}

// ptahNamed copies the migrations of dir into a new directory under Ptah's
// own names, 0000000001_create.up.sql for 1_create.up.sql, where a file named
// R__<identity>.sql is a repeatable migration. The names of dir are read in
// the Atlas format, where R__<identity>.sql is the one-time version R, which
// the engine's revision table cannot record.
func ptahNamed(c *qt.C, dir string) string {
	c.Helper()
	entries, err := os.ReadDir(dir)
	c.Assert(err, qt.IsNil)
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(dir, entry.Name())) // #nosec G304 -- a file of the test's own c.TempDir()
		c.Assert(err, qt.IsNil)
		spelled, name, found := strings.Cut(entry.Name(), "_")
		c.Assert(found, qt.IsTrue, qt.Commentf("%s", entry.Name()))
		version, err := strconv.ParseInt(spelled, 10, 64)
		c.Assert(err, qt.IsNil)
		files[fmt.Sprintf("%010d_%s", version, name)] = string(body)
	}
	return writeFiles(c, c.TempDir(), files)
}

// The built-in YDB engine's log layout has no column for a repeatable
// migration's identity. On a database that engine migrated, and whose
// migrations a run has resumed and completed, a repeatable migration still
// runs; the run document names both log entries it could not write, and the
// engine's log gains no entry that would read as one about version 0.
func TestHostRunsARepeatableMigrationBesideTheBuiltInEnginesLog_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.RepeatableRecordFeature)
	opened, _, database := lockFixture(c)
	builtinHistory(c, opened, database)
	recorded := builtinMigrations(c)
	scope := []string{"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--migrations-schema", builtinHistoryDirectory}
	resumed, resumeErr := hostCommand(c, append([]string{"migrations", "up", "--migrations-dir", recorded, "--allow-dirty"}, scope...)...)
	c.Assert(resumeErr, qt.IsNil, qt.Commentf("%s", resumed))
	migrations := ptahNamed(c, recorded)
	writeFiles(c, migrations, map[string]string{"R__marker.sql": "UPSERT INTO `" + builtinHistoryDirectory + "/items` (id) VALUES (900);\n"})

	var run repeatableRun
	stderr, err := hostDocument(c, &run, append([]string{"migrations", "up", "--json", "--migrations-dir", migrations}, scope...)...)
	var unversioned uint64
	// #nosec G202 -- a fixed fixture table under the database root
	countErr := opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+path.Join(database, builtinHistoryDirectory, "schema_migrations_log")+
		"` WHERE version = 0l").Scan(&unversioned)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", stderr))
	c.Assert(run.Applied, qt.HasLen, 0)
	c.Assert(run.AppliedRepeatables, qt.DeepEquals, []string{"marker"})
	c.Assert(run.lost(), qt.DeepEquals, []string{"up marker", "up marker"})
	c.Assert(countErr, qt.IsNil)
	c.Assert(unversioned, qt.Equals, uint64(0))
}

// recordDeclaration declares, in the fixture's directory, the tables
// repeatableMigrations creates, followed by further.
func recordDeclaration(f migrationsFixture, further string) string {
	return "tables:\n" +
		"  users:\n    schema: " + f.dir + "\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"  runs:\n    schema: " + f.dir + "\n    columns:\n      tag: {type: Utf8, primary: true, not_null: true}\n" + further
}

// appliedRecordFixture is a fixture whose up run applied repeatableMigrations,
// under a host that reads the record of repeatable migrations as reserved
// storage.
func appliedRecordFixture(c *qt.C) migrationsFixture {
	c.Helper()
	requireHostNegotiates(c, provider.RepeatableRecordFeature)
	requireHostNegotiates(c, provider.ReservedRepeatableRecordFeature)
	f := newMigrationsFixture(c)
	run, stderr, err := f.up(c, repeatableMigrations(c, f))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", stderr))
	c.Assert(run.AppliedRepeatables, qt.DeepEquals, []string{"user_names"})
	c.Assert(f.names(c), qt.Contains, "schema_migrations_repeatable")
	return f
}

// The record of repeatable migrations is this provider's storage: a plan over
// the directory that declares the tables the migrations created has nothing to
// run, and drops neither the record nor its row.
func TestHostLeavesTheRepeatableRecordOutOfAPlan_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := appliedRecordFixture(c)

	planned := hostPlanned(c, f.dir, "yaml", recordDeclaration(f, ""))

	c.Assert(planned, qt.HasLen, 0)
	c.Assert(f.names(c), qt.Contains, "schema_migrations_repeatable")
}

// A declaration that takes the record's name is refused before anything runs,
// and the record keeps the content that applied, so an unchanged rerun has
// nothing to run.
func TestHostRefusesADeclarationOfTheRepeatableRecord_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := appliedRecordFixture(c)
	declared := recordDeclaration(f, "  schema_migrations_repeatable:\n    schema: "+f.dir+"\n    columns:\n"+
		"      identity: {type: Utf8, primary: true, not_null: true}\n")

	applied, err := hostApplySelected(c, f.dir, "yaml", declared)
	rerun, stderr, rerunErr := f.up(c, repeatableMigrations(c, f))

	c.Assert(err, qt.IsNotNil)
	c.Assert(applied, qt.Contains, `schema_migrations_repeatable is reserved for the record of repeatable migrations by provider "github.com/stokaro/ptah-ydb-plugin"`)
	c.Assert(rerunErr, qt.IsNil, qt.Commentf("%s", stderr))
	c.Assert(rerun.Outcome, qt.Equals, "up-to-date")
	c.Assert(f.runs(c), qt.DeepEquals, []string{"v1"})
}
