//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// cliRealm is a dev realm of the test database that a host command takes as a
// database of its own: its address, its root and a connection to the test
// database. Everything in it, and the realm, is removed when the test ends. A
// command that reaches the whole of its database, such as `db drop-all`, runs
// against a realm so it leaves the rest of a shared server alone.
type cliRealm struct {
	address string
	root    string
	opened  *connection.Connection
}

func newCLIRealm(c *qt.C) cliRealm {
	c.Helper()
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	name := "r" + strings.ToLower(rand.Text())[:12]
	parsed, err := url.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	query := parsed.Query()
	query.Set("dev_realm", name)
	parsed.RawQuery = query.Encode()
	realm := cliRealm{address: parsed.String(), root: path.Join(base.Database(), connection.RealmDirectory, name), opened: opened}
	c.Assert(opened.Driver.Scheme().MakeDirectory(ctx, realm.root), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		removeTree(cleanup, opened, realm.root)
		// Another test may hold a realm of its own, which keeps the directory.
		_ = opened.Driver.Scheme().RemoveDirectory(cleanup, path.Join(base.Database(), connection.RealmDirectory))
	})
	return realm
}

// entries lists the realm's directory at relative, by name and type; nil when
// YDB shows no such directory.
func (r cliRealm) entries(c *qt.C, relative ...string) map[string]Ydb_Scheme.Entry_Type {
	c.Helper()
	return heldEntries(c, r.opened, path.Join(append([]string{r.root}, relative...)...))
}

// The schema the binary applies, in Go annotations, so the run goes through the
// host's parser and this provider's planner and writer the way an operator's
// does: an auto-increment BIGINT key, a VARCHAR with a default and an index, in
// a nested directory.
const cliEntities = `package entities

//ptah:schema:table name="items" schema="ptah_ydb_e2e/nested"
type Item struct {
	//ptah:schema:field name="id" type="BIGINT" primary auto_increment
	ID int64
	//ptah:schema:field name="title" type="VARCHAR(80)" not_null default="untitled"
	Title string
	//ptah:schema:field name="created_at" type="TIMESTAMP"
	CreatedAt string
	//ptah:schema:index name="idx_items_title" fields="title"
	_ int
}
`

// `schema apply` builds the declared table in a nested directory, `schema
// compare` finds nothing left to change, `db read` writes the table back as
// the YQL that creates it, and `db drop-all` drops it and removes the
// directories it emptied. The run is in a dev realm, which the commands read as
// a database of their own.
func TestHostAppliesComparesAndDropsASchema_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": cliEntities})

	applied, applyErr := hostCommand(c, "schema", "apply", "--db-url", realm.address, "--root-dir", entities, "--auto-approve")
	held := realm.entries(c, "ptah_ydb_e2e", "nested")
	compared, compareErr := hostCommand(c, "schema", "compare", "--db-url", realm.address, "--root-dir", entities,
		"--schemas", "ptah_ydb_e2e/nested", "--exit-code")
	read, readErr := hostCommand(c, "db", "read", "--db-url", realm.address, "--schemas", "ptah_ydb_e2e/nested")
	dropped, dropErr := hostCommand(c, "db", "drop-all", "--db-url", realm.address, "--auto-approve")

	c.Assert(applyErr, qt.IsNil, qt.Commentf("schema apply:\n%s", applied))
	c.Assert(held, qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"items": Ydb_Scheme.Entry_TABLE})
	c.Assert(compareErr, qt.IsNil, qt.Commentf("schema compare:\n%s", compared))
	c.Assert(compared, qt.Contains, "No schema differences detected")
	c.Assert(readErr, qt.IsNil, qt.Commentf("db read:\n%s", read))
	c.Assert(read, qt.Contains, "CREATE TABLE `ptah_ydb_e2e/nested/items` (")
	c.Assert(read, qt.Contains, "`id` BigSerial NOT NULL,")
	c.Assert(read, qt.Contains, "`title` Utf8 NOT NULL DEFAULT 'untitled'u,")
	c.Assert(read, qt.Contains, "INDEX `idx_items_title` GLOBAL SYNC ON (`title`)")
	c.Assert(dropErr, qt.IsNil, qt.Commentf("db drop-all:\n%s", dropped))
	c.Assert(entryNames(realm.entries(c)), qt.Not(qt.Contains), "ptah_ydb_e2e")
}

// `db verify` runs an assertion that holds and the run exits 0.
func TestHostVerifiesAHoldingCheck_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	checks := writeFiles(c, c.TempDir(), map[string]string{"holds.sql": `-- +ptah check name="holds" assert="SELECT 1 = 1"` + "\n"})

	verified, err := hostCommand(c, "db", "verify", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--checks", filepath.Join(checks, "holds.sql"))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", verified))
	c.Assert(verified, qt.Contains, "Verdict: verified (1 verified, 0 failed, 0 errored of 1)")
}

// `db verify` reports an assertion that does not hold as failed and refuses a
// write before it runs, as errored, which is the exit status a fault gets.
func TestHostVerifiesChecks_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	table := path.Join(directory, "ptah_ydb_verify")
	checks := writeFiles(c, c.TempDir(), map[string]string{"mixed.sql": `-- +ptah check name="holds" assert="SELECT 1 = 1"` + "\n" +
		`-- +ptah check name="fails" assert="SELECT 1 = 2"` + "\n" +
		"-- +ptah check name=\"write\" assert=\"UPSERT INTO `" + table + "` (`id`) VALUES (1)\"\n"})

	reported, err := hostCommand(c, "db", "verify", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--checks", filepath.Join(checks, "mixed.sql"))

	c.Assert(err, qt.ErrorMatches, "exit status 2")
	c.Assert(reported, qt.Contains, "Verdict: errored (1 verified, 1 failed, 1 errored of 3)")
	c.Assert(reported, qt.Contains, "reason: check assertion must be a read-only SELECT statement")
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.HasLen, 0)
}

// `schema apply` locks on the coordination node the first locking run creates,
// and a dry run writes nothing to the database, the node included. The read
// after the real run reserves the node rather than describing it, so no plan
// offers to drop it.
func TestHostSchemaApplyDryRunCreatesNoLockNode_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "lock_items")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	source := "package entities\n\n//ptah:schema:table name=\"lock_items\" schema=\"" + directory + "\"\n" +
		"type LockItem struct {\n\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary\n\tID int64\n}\n"
	entities := writeFiles(c, c.TempDir(), map[string]string{"items.go": source})
	root := func() []string { return entryNames(heldEntries(c, opened, database)) }
	before := root()

	previewed, previewErr := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", entities,
		"--schemas", directory, "--dry-run")
	planned := hostPlanned(c, directory, "go", source)
	afterPreview := root()
	applied, applyErr := hostCommand(c, "schema", "apply", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--root-dir", entities,
		"--schemas", directory, "--auto-approve", "--tx-mode", "none")
	afterApply := root()
	binary, digest := providerBinary(c)
	client, session, _ := historySession(c, binary, digest)
	snapshot, inspectErr := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session, Schemas: []string{"/"}})

	c.Assert(previewErr, qt.IsNil, qt.Commentf("schema apply --dry-run:\n%s", previewed))
	c.Assert(planned, qt.DeepEquals, []string{"row-table-create-operation " + directory + ".lock_items"})
	c.Assert(afterPreview, qt.DeepEquals, before)
	c.Assert(before, qt.Not(qt.Contains), locking.NodeName)
	c.Assert(applyErr, qt.IsNil, qt.Commentf("schema apply:\n%s", applied))
	c.Assert(afterApply, qt.Contains, locking.NodeName)
	c.Assert(inspectErr, qt.IsNil)
	c.Assert(slices.ContainsFunc(snapshot.NativeObjects, func(value *protocol.Envelope) bool { return value.GetOwner().GetName() == locking.NodeName }), qt.IsFalse)
	c.Assert(slices.ContainsFunc(snapshot.ReservedObjects, func(reserved *protocol.ReservedObject) bool {
		return reserved.GetObject().GetName() == locking.NodeName && reserved.GetObject().GetKind() == schema.CoordinationProperties &&
			reserved.GetPurpose() == protocol.ReservedObject_LOCK
	}), qt.IsTrue)
}

// `migrations checkpoint --data-table` writes a checkpoint whose reference
// table carries its rows, with the test database as the shadow database, and a
// fresh database bootstraps from it. YQL types every literal, so each value is
// written in the type the replayed table gives its column. The rows the fresh
// database reads back are the rows the history produced, a Decimal and a
// Timestamp included, and the shadow leaves nothing behind.
func TestHostCheckpointCarriesReferenceRows_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, _, database := lockFixture(c)
	fresh := newCLIRealm(c)
	migrations := writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_colors.up.sql": "CREATE TABLE `ref/colors` (`id` Int32 NOT NULL, `name` Utf8, " +
			"`weight` Decimal(22,9), `created` Timestamp, PRIMARY KEY (`id`));\n",
		"0000000001_colors.down.sql": "DROP TABLE `ref/colors`;\n",
		"0000000002_rows.up.sql": "UPSERT INTO `ref/colors` (`id`, `name`, `weight`, `created`) VALUES " +
			"(2, 'blue'u, Decimal('1.5', 22, 9), Timestamp('2026-01-02T03:04:05Z')), " +
			"(1, 'red'u, Decimal('0.25', 22, 9), Timestamp('2025-12-31T00:00:00Z'));\n",
		"0000000002_rows.down.sql": "DELETE FROM `ref/colors`;\n",
	})
	hashed, hashErr := hostOffline(c, "migrations", "hash", "--dir", migrations)
	c.Assert(hashErr, qt.IsNil, qt.Commentf("%s", hashed))

	written, writeErr := hostCommand(c, "migrations", "checkpoint", "--migrations-dir", migrations,
		"--shadow-db", os.Getenv("PTAH_YDB_TEST_URL"), "--data-table", "ref.colors")
	c.Assert(writeErr, qt.IsNil, qt.Commentf("%s", written))
	applied, upErr := hostCommand(c, "migrations", "up", "--db-url", fresh.address, "--migrations-dir", migrations)
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s", applied))
	rows, err := fresh.opened.DB.QueryContext(c.Context(),
		"SELECT `name`, CAST(`weight` AS Utf8), CAST(`created` AS Utf8) FROM `"+path.Join(fresh.root, "ref", "colors")+"` ORDER BY `id`")
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(rows.Close(), qt.IsNil) }()
	var read []string
	for rows.Next() {
		var name, weight, created string
		c.Assert(rows.Scan(&name, &weight, &created), qt.IsNil)
		read = append(read, name+" "+weight+" "+created)
	}
	c.Assert(rows.Err(), qt.IsNil)
	checkpoints, globErr := filepath.Glob(filepath.Join(migrations, "*.checkpoint.up.sql"))
	c.Assert(globErr, qt.IsNil)
	c.Assert(checkpoints, qt.HasLen, 1)
	body, readErr := os.ReadFile(checkpoints[0])

	c.Assert(readErr, qt.IsNil)
	c.Assert(string(body), qt.Contains, "-- ptah:checkpoint-data ref.colors rows=2")
	c.Assert(read, qt.DeepEquals, []string{"red 0.25 2025-12-31T00:00:00Z", "blue 1.5 2026-01-02T03:04:05Z"})
	c.Assert(entryNames(heldEntries(c, fresh.opened, database)), qt.Not(qt.Contains), "ref")
	c.Assert(heldEntries(c, fresh.opened, path.Join(database, connection.RealmDirectory)), qt.HasLen, 1)
}

// A table the plan creates is assessed safe and leaves access unchanged, as
// the built-in engine assesses a CREATE TABLE: no role could read the table
// before the plan, and it takes its directory's permissions. The host reads an
// unknown access effect as destructive.
func TestHostAssessesACreatedTableSafe_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, _ := inspectionFixture(c)

	units := hostPlannedUnits(c, directory, "yaml", "tables:\n  created_items:\n    schema: "+directory+"\n    columns:\n      id: {type: Uint64, primary: true}\n")

	c.Assert(units, qt.HasLen, 1)
	c.Assert(units[0].Native.Subject, qt.Equals, directory+".created_items")
	c.Assert(units[0].Native.Severity, qt.Equals, "safe")
	c.Assert(units[0].Native.Access, qt.Equals, "unchanged")
	c.Assert(units[0].Native.AccessReason, qt.Equals, "The table is created by this plan, so no role could read it before; it takes its directory's permissions")
}
