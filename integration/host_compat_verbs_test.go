//go:build integration

package integration_test

import (
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// compatDirectory is the directory of a realm the compatibility tests put a
// table in, beside compatRootTable at the realm's root.
const (
	compatDirectory = "ptah_ydb_compat"
	compatRootTable = "ptah_ydb_compat_root"
)

// compatDesired is a desired schema in HCL with a table at the root and one
// in compatDirectory, the shape whose read scope a document's schema block
// must not narrow.
const compatDesired = `table "` + compatRootTable + `" {
  column "id" {
    type = Int64
  }
  column "note" {
    type = Utf8
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}

schema "` + compatDirectory + `" {
}

table "orders" {
  schema = schema.` + compatDirectory + `
  column "id" {
    type = Uint64
  }
  column "total" {
    type = Decimal(22,9)
  }
  primary_key {
    columns = [column.id]
  }
  index "orders_total_ix" {
    type = "GLOBAL SYNC"
    columns = [column.total]
  }
}
`

// compatMigrations are two migrations of a table in compatDirectory; the
// second carries the down section migrate down runs.
var compatMigrations = map[string]string{
	"20260101000000_items.sql": "CREATE TABLE `" + compatDirectory + "/items` (`id` Int64 NOT NULL, PRIMARY KEY (`id`));\n",
	"20260102000000_name.sql": "-- atlas:txtar\n\n-- migration.sql --\n" +
		"ALTER TABLE `" + compatDirectory + "/items` ADD COLUMN `name` Utf8;\n\n" +
		"-- down.sql --\n" +
		"ALTER TABLE `" + compatDirectory + "/items` DROP COLUMN `name`;\n",
}

// compatScripts updates the root table's row, once without and once with a
// row-count assertion.
const compatScripts = `script "exec" "note" {
  exec "set" {
    sql = "UPDATE ` + "`" + compatRootTable + "`" + ` SET note = 'set'u WHERE id = 1"
  }
}

script "exec" "checked" {
  exec "set" {
    sql         = "UPDATE ` + "`" + compatRootTable + "`" + ` SET note = 'checked'u WHERE id = 1"
    expect_rows = 1
  }
}
`

// compatMigrationsDir writes compatMigrations to a directory of its own and
// returns its file:// URL.
func compatMigrationsDir(c *qt.C) string {
	c.Helper()
	dir := c.TempDir()
	for name, body := range compatMigrations {
		compatFile(c, dir, name, body)
	}
	return "file://" + filepath.ToSlash(dir)
}

// Ported from the host's TestYDBCompatBinary_RunsTheAtlasVerbs: ptah-compat
// runs each Atlas verb through this provider in a dev realm, which the verbs
// read as a database of their own. schema apply plans the whole database, as
// the Atlas verb does, so it drops what the document does not declare, and
// the realm is what leaves the rest of a shared server alone. schema apply
// builds a table at the root and one in a directory and then finds them
// synced; schema inspect describes both, as HCL that applies back as synced
// and as YQL, and names neither the lock node nor, once migrate apply wrote
// it, the revision table as an object it left out; schema diff finds nothing
// to change; migrate apply records two migrations in the Atlas revision table
// in the directory, and migrate down rolls one back; script exec writes a row
// and refuses expect_rows, because the driver's row count is not a
// measurement.
func TestHostCompatRunsTheAtlasVerbs_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	realm := newCLIRealm(c)
	work := c.TempDir()
	desired := "file://" + compatFile(c, work, "desired.hcl", compatDesired)
	scripts := compatFile(c, work, "scripts.hcl", compatScripts)
	dir := compatMigrationsDir(c)
	revisions := path.Join(realm.root, compatDirectory, "atlas_schema_revisions")
	root := path.Join(realm.root, compatRootTable)

	applied, applyNotes, applyExit := runCompat(c, nil, "schema", "apply", "--url", realm.address, "--to", desired, "--auto-approve")
	synced, _, syncedExit := runCompat(c, nil, "schema", "apply", "--url", realm.address, "--to", desired, "--dry-run")
	inspected, inspectNotes, inspectExit := runCompat(c, nil, "schema", "inspect", "--url", realm.address)
	reapplied, _, reapplyExit := runCompat(c, nil, "schema", "apply", "--url", realm.address,
		"--to", "file://"+compatFile(c, work, "inspected.hcl", inspected), "--dry-run")
	yql, yqlNotes, yqlExit := runCompat(c, nil, "schema", "inspect", "--url", realm.address, "--format", "{{ sql . }}")
	diffed, diffNotes, diffExit := runCompat(c, nil, "schema", "diff", "--from", realm.address, "--to", desired, "--dev-url", realm.address)
	hashed, hashNotes, hashExit := runCompat(c, nil, "migrate", "hash", "--dir", dir)
	migrated, migrateNotes, migrateExit := runCompat(c, nil, "migrate", "apply", "--url", realm.address, "--dir", dir,
		"--revisions-schema", compatDirectory)
	status, _, statusExit := runCompat(c, nil, "migrate", "status", "--url", realm.address, "--dir", dir, "--revisions-schema", compatDirectory)
	recorded := rowCount(c, realm.opened, revisions, "")
	described, describedNotes, describedExit := runCompat(c, nil, "schema", "inspect", "--url", realm.address, "--schema", compatDirectory)
	rolledBack, downNotes, downExit := runCompat(c, nil, "migrate", "down", "--url", realm.address, "--dir", dir,
		"--revisions-schema", compatDirectory, "--to-version", "20260101000000")
	_, err := realm.opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+root+"` (id, note) VALUES (1l, 'start'u)")
	c.Assert(err, qt.IsNil)
	ran, ranNotes, runExit := runCompat(c, nil, "script", "exec", "--url", realm.address, "--file", scripts, "--run", "note", "--quiet")
	_, refused, refusedExit := runCompat(c, nil, "script", "exec", "--url", realm.address, "--file", scripts, "--run", "checked")

	c.Assert(applyExit, qt.Equals, 0, qt.Commentf("schema apply:\n%s\n%s", applied, applyNotes))
	c.Assert(applied, qt.Contains, "Schema apply completed successfully.")
	c.Assert(syncedExit, qt.Equals, 0, qt.Commentf("schema apply again:\n%s", synced))
	c.Assert(synced, qt.Equals, "Schema is synced, no changes to be made\n")
	c.Assert(inspectExit, qt.Equals, 0, qt.Commentf("schema inspect:\n%s", inspectNotes))
	c.Assert(inspected, qt.Contains, "table \""+compatRootTable+"\" {\n  column \"id\" {\n    type = Int64\n  }")
	c.Assert(inspected, qt.Contains, "table \"orders\" {\n  schema = schema."+compatDirectory+"\n")
	c.Assert(inspected, qt.Contains, "    type = Decimal(22,9)\n")
	// The lock node and the revision table are the provider's storage at
	// reserved names; the document names neither as an object it left out.
	c.Assert(inspected, qt.Not(qt.Contains), "ptah:not-described")
	c.Assert(reapplyExit, qt.Equals, 0, qt.Commentf("schema apply of the inspected document:\n%s", reapplied))
	c.Assert(reapplied, qt.Equals, "Schema is synced, no changes to be made\n")
	c.Assert(yqlExit, qt.Equals, 0, qt.Commentf("schema inspect --format sql:\n%s", yqlNotes))
	c.Assert(yql, qt.Contains, "CREATE TABLE `"+compatDirectory+"/orders` (\n    `id` Uint64 NOT NULL,\n")
	c.Assert(diffExit, qt.Equals, 0, qt.Commentf("schema diff:\n%s\n%s", diffed, diffNotes))
	c.Assert(diffed, qt.Equals, "Schemas are synced, no changes to be made.\n")
	c.Assert(hashExit, qt.Equals, 0, qt.Commentf("migrate hash:\n%s\n%s", hashed, hashNotes))
	c.Assert(migrateExit, qt.Equals, 0, qt.Commentf("migrate apply:\n%s\n%s", migrated, migrateNotes))
	c.Assert(statusExit, qt.Equals, 0, qt.Commentf("migrate status:\n%s", status))
	c.Assert(status, qt.Contains, "Migration Status: OK")
	c.Assert(recorded, qt.Equals, int64(2))
	c.Assert(describedExit, qt.Equals, 0, qt.Commentf("schema inspect --schema:\n%s", describedNotes))
	c.Assert(described, qt.Not(qt.Contains), "atlas_schema_revisions")
	c.Assert(downExit, qt.Equals, 0, qt.Commentf("migrate down:\n%s\n%s", rolledBack, downNotes))
	c.Assert(rowCount(c, realm.opened, revisions, ""), qt.Equals, int64(1))
	c.Assert(heldColumnTypes(c, realm.opened, path.Join(realm.root, compatDirectory, "items")), qt.DeepEquals, map[string]string{"id": "Int64"})
	c.Assert(runExit, qt.Equals, 0, qt.Commentf("script exec:\n%s\n%s", ran, ranNotes))
	c.Assert(refusedExit, qt.Not(qt.Equals), 0)
	c.Assert(refused, qt.Contains, `expect_rows is 1, and this driver does not report a row count`)
	c.Assert(rowCount(c, realm.opened, root, "id = 1l AND note = 'set'u"), qt.Equals, int64(1))
}

// compatRebuildDesired declares table rb of directory with its column v
// widened from Int32 to Int64, a change YDB makes only by rebuilding the
// table.
func compatRebuildDesired(directory string) string {
	return `schema "` + directory + `" {
}

table "rb" {
  schema = schema.` + directory + `
  column "id" {
    type = Int64
  }
  column "v" {
    type = Int64
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}
`
}

// compatRebuildFixture is table rb of a fixture directory, created with v as
// an Int32 and given two rows, the HCL that widens v, and the schema apply
// arguments that apply it to the directory.
func compatRebuildFixture(c *qt.C) (f migrationsFixture, absolute string, apply []string) {
	c.Helper()
	opened, directory, database := lockFixture(c)
	absolute = path.Join(database, directory, "rb")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` (`id` Int64 NOT NULL, `v` Int32, PRIMARY KEY (`id`))"), qt.IsNil)
	_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, v) VALUES (1l, 10), (2l, NULL)")
	c.Assert(err, qt.IsNil)
	desired := "file://" + compatFile(c, c.TempDir(), "desired.hcl", compatRebuildDesired(directory))
	apply = []string{"schema", "apply", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", directory, "--to", desired}
	return migrationsFixture{opened: opened, dir: directory, database: database}, absolute, apply
}

// Ported from the host's TestYDBCompatBinary_RebuildsATableOnRequest.
// ptah-compat takes no flag the community binary lacks, so it asks for a
// table rebuild with PTAH_ALLOW_TABLE_REBUILD where native ptah takes
// --allow-table-rebuild. With the variable, schema apply rebuilds the table,
// keeps its rows, and the next run finds nothing to change.
func TestHostCompatRebuildsATableOnRequest_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	f, absolute, apply := compatRebuildFixture(c)

	rebuilt, notes, rebuildExit := runCompat(c, []string{"PTAH_ALLOW_TABLE_REBUILD=1"}, append(apply, "--auto-approve")...)
	synced, _, syncedExit := runCompat(c, nil, append(apply, "--dry-run")...)

	c.Assert(rebuildExit, qt.Equals, 0, qt.Commentf("schema apply:\n%s\n%s", rebuilt, notes))
	c.Assert(rebuilt, qt.Contains, "Schema apply completed successfully.")
	c.Assert(heldColumnTypes(c, f.opened, absolute)["v"], qt.Equals, "Optional<Int64>")
	c.Assert(f.names(c), qt.DeepEquals, []string{"rb"})
	c.Assert(rowCount(c, f.opened, absolute, ""), qt.Equals, int64(2))
	c.Assert(rowCount(c, f.opened, absolute, "id = 1l AND v = 10l"), qt.Equals, int64(1))
	c.Assert(syncedExit, qt.Equals, 0, qt.Commentf("schema apply again:\n%s", synced))
	c.Assert(synced, qt.Equals, "Schema is synced, no changes to be made\n")
}

// Without PTAH_ALLOW_TABLE_REBUILD the type change is refused, the refusal
// names the variable, and the table keeps its type. Strict mode refuses the
// variable before any command runs.
func TestHostCompatRefusesARebuildNotAskedFor_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	f, absolute, apply := compatRebuildFixture(c)

	_, refused, refusedExit := runCompat(c, nil, append(apply, "--auto-approve")...)
	_, strict, strictExit := runCompat(c, []string{"PTAH_ATLAS_STRICT_COMPAT=1", "PTAH_ALLOW_TABLE_REBUILD=1"}, append(apply, "--auto-approve")...)

	c.Assert(refusedExit, qt.Not(qt.Equals), 0)
	c.Assert(refused, qt.Contains, "which Ptah plans when asked with PTAH_ALLOW_TABLE_REBUILD=1")
	c.Assert(strictExit, qt.Not(qt.Equals), 0)
	c.Assert(strict, qt.Equals, "Error: PTAH_ATLAS_STRICT_COMPAT does not allow PTAH_ALLOW_TABLE_REBUILD\n")
	c.Assert(heldColumnTypes(c, f.opened, absolute)["v"], qt.Equals, "Optional<Int32>")
}

// compatLockName is the lock ptah-compat schema apply takes, a semaphore on
// the lock node at the root of the database its URL names.
const compatLockName = "ptah_schema_apply"

// Ported from the host's TestYDBCompatBinary_SchemaApplyWaitsForTheLock:
// ptah-compat schema apply takes the schema apply lock on YDB, a semaphore on
// the coordination node, as it takes an advisory lock on the engines that
// have one. While another session holds it, a run under --lock-timeout gives
// up with the timeout and creates nothing.
func TestHostCompatSchemaApplyWaitsForTheLock_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	realm := newCLIRealm(c)
	desired := "file://" + compatFile(c, c.TempDir(), "desired.hcl", compatDesired)
	hold, busy, err := locking.Try(c.Context(), realm.opened.Driver, realm.root, compatLockName)
	c.Assert(err, qt.IsNil)
	c.Assert(busy, qt.IsFalse)
	c.Cleanup(func() { c.Check(hold.Close(context.Background()), qt.IsNil) })

	_, stderr, exit := runCompat(c, nil, "schema", "apply", "--url", realm.address, "--to", desired, "--auto-approve", "--lock-timeout", "1s")

	c.Assert(exit, qt.Not(qt.Equals), 0)
	c.Assert(stderr, qt.Equals, "Error: acquire schema apply lock: timed out acquiring advisory lock \""+compatLockName+"\" on ydb after 1s\n")
	c.Assert(realm.entries(c), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{locking.NodeName: Ydb_Scheme.Entry_COORDINATION_NODE})
}

// Ported from the host's TestYDBCompatBinary_SchemaApplyStopsWhenTheLockIsLost:
// ptah-compat schema apply holds the schema apply lock while it plans, asks
// and applies. Here the lock node is dropped while the command waits for its
// confirmation, which ends the session that held the semaphore. Once
// confirmed, the command applies nothing: another run may have taken the lock
// and changed the schema it planned against, so it reports the lost lock and
// creates no table.
func TestHostCompatSchemaApplyStopsWhenTheLockIsLost_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	binary, manifest := hostCompat(c)
	realm := newCLIRealm(c)
	desired := "file://" + compatFile(c, c.TempDir(), "desired.hcl", compatDesired)
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()
	cmd := compatCommand(ctx, binary, manifest, nil, "schema", "apply", "--url", realm.address, "--to", desired)
	stdin, err := cmd.StdinPipe()
	c.Assert(err, qt.IsNil)
	var stdout, stderr lockedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	c.Assert(cmd.Start(), qt.IsNil)

	asked := stdout.waitFor("Type 'YES' to confirm", 2*time.Minute)
	dropped := realm.opened.Driver.Coordination().DropNode(ctx, path.Join(realm.root, locking.NodeName))
	// The holder trusts the server's last answer that it holds the semaphore
	// for half the session timeout; give it longer than that before the
	// command goes on.
	time.Sleep(8 * time.Second)
	_, answerErr := io.WriteString(stdin, "YES\n")
	closeErr := stdin.Close()
	waitErr := cmd.Wait()

	c.Assert(asked, qt.IsTrue, qt.Commentf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String()))
	c.Assert(dropped, qt.IsNil)
	c.Assert(answerErr, qt.IsNil)
	c.Assert(closeErr, qt.IsNil)
	c.Assert(waitErr, qt.ErrorMatches, "exit status [1-9][0-9]*", qt.Commentf("stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String()))
	c.Assert(stderr.String(), qt.Contains, `schema apply lock: advisory lock "`+compatLockName+`" on ydb was lost while it was held`)
	c.Assert(stdout.String(), qt.Not(qt.Contains), "Schema apply completed successfully.")
	c.Assert(realm.entries(c), qt.HasLen, 0)
}

// lockedOutput is a stream a running command writes to while the test reads
// it.
type lockedOutput struct {
	mu   sync.Mutex
	text strings.Builder
}

func (o *lockedOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.Write(p)
}

func (o *lockedOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

// waitFor reports whether the stream comes to hold text within limit.
func (o *lockedOutput) waitFor(text string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if strings.Contains(o.String(), text) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// compatLoop is the loop of stokaro/ptah#4127 over table, in the spelling a
// do body reads the cursor with. YDB types its parameters, so it measures
// what a permissive engine cannot: the literal 100 has to reach the server as
// an Int64 to add to an Int64 column, and the cursor's id has to reach it as
// one to match a key. A script writes its engine's own placeholder, and on
// YDB the connection binds the first argument to $p1.
func compatLoop(table string) string {
	return strings.ReplaceAll(`script "loop" "touch" {
  iterator "keyset" {
    cursor {
      id = int
    }
    init {
      sql = "SELECT id FROM `+"`{table}`"+` ORDER BY id LIMIT 1"
    }
    next {
      sql  = "SELECT id FROM `+"`{table}`"+` WHERE id > $p1 ORDER BY id LIMIT 1"
      args = [cursor.id]
    }
  }
  do {
    exec "touch" {
      sql  = "UPDATE `+"`{table}`"+` SET price = price + $p1 WHERE id = $p2"
      args = [100, iterator.keyset.cursor.id]
    }
  }
}
`, "{table}", table)
}

// Ported from the host's TestYDBCompatBinary_ScriptBindsTypedArgs: a loop
// script through ptah-compat adds 100 to the row its cursor names in every
// batch, so both rows end at 100. Bound as text, the constant would be a type
// error on the server and the cursor would match no key.
func TestHostCompatScriptBindsTypedArgs_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	opened, directory, database := inspectionFixture(c)
	absolute := path.Join(database, directory, "script_args")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` (id Int64 NOT NULL, price Int64 NOT NULL, PRIMARY KEY (id))"), qt.IsNil)
	_, err := opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, price) VALUES (1l, 0l), (2l, 0l)")
	c.Assert(err, qt.IsNil)
	script := compatFile(c, c.TempDir(), "script.hcl", compatLoop(directory+"/script_args"))

	ran, report, exit := runCompat(c, nil, "script", "loop", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--file", script)

	c.Assert(exit, qt.Equals, 0, qt.Commentf("script loop:\n%s\n%s", ran, report))
	c.Assert(report, qt.Contains, "-- 2 batches, 2 rows")
	c.Assert(rowCount(c, opened, absolute, "price = 100l"), qt.Equals, int64(2))
}
