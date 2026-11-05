//go:build integration

package integration_test

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
)

// hostCommand runs one command of the pinned host with this provider selected
// for ydb, and returns what it wrote to both streams and the error it exited
// with. The arguments name every address the command takes, so a command can
// name the test database, a dev realm in it, or both.
func hostCommand(c *qt.C, args ...string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	return runHost(c, host, append(args, "--provider-config", manifest, "--provider-engine", "ydb")...)
}

// hostOffline runs a command of the pinned host that connects to nothing, such
// as `migrations hash`, which takes no provider flags.
func hostOffline(c *qt.C, args ...string) (string, error) {
	c.Helper()
	host, _ := hostManifest(c)
	return runHost(c, host, args...)
}

func runHost(c *qt.C, host string, args ...string) (string, error) {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, host, args...).CombinedOutput()
	return string(output), err
}

// writeFiles writes each body into dir under its name and returns dir.
func writeFiles(c *qt.C, dir string, files map[string]string) string {
	c.Helper()
	c.Assert(os.MkdirAll(dir, 0o750), qt.IsNil)
	for name, body := range files {
		c.Assert(os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600), qt.IsNil)
	}
	return dir
}

// migrationsFixture is a directory the host's migrations write their tables
// and their revision table to, below a directory of lockFixture's, so the lock
// node a run creates goes with the test. Everything below it is removed when
// the test ends.
type migrationsFixture struct {
	opened *connection.Connection
	// dir is the directory relative to the database root, as a migration
	// names it and --migrations-schema takes it.
	dir      string
	database string
}

func newMigrationsFixture(c *qt.C) migrationsFixture {
	c.Helper()
	opened, directory, database := lockFixture(c)
	fixture := migrationsFixture{opened: opened, dir: path.Join(directory, "m"), database: database}
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		removeTree(cleanup, opened, path.Join(database, fixture.dir))
	})
	return fixture
}

// table is the path a migration names a table of the fixture's directory by.
func (f migrationsFixture) table(name string) string { return path.Join(f.dir, name) }

// names lists the fixture's directory, the revision tables included.
func (f migrationsFixture) names(c *qt.C) []string {
	c.Helper()
	return entryNames(heldEntries(c, f.opened, path.Join(f.database, f.dir)))
}

// entryNames is the names of a directory's entries, in order, so an assertion
// can say which names a directory holds.
func entryNames(held map[string]Ydb_Scheme.Entry_Type) []string {
	names := make([]string, 0, len(held))
	for name := range held {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// count reads one count with the fixture's connection.
func (f migrationsFixture) count(c *qt.C, query string) int64 {
	c.Helper()
	var value uint64
	c.Assert(f.opened.DB.QueryRowContext(c.Context(), query).Scan(&value), qt.IsNil, qt.Commentf("query: %s", query))
	return int64(value)
}

// target is the flags every versioned verb takes to reach the fixture's
// revision table on the test database.
func (f migrationsFixture) target(migrations string, extra ...string) []string {
	return append([]string{"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--migrations-dir", migrations, "--migrations-schema", f.dir}, extra...)
}

// progress is what a test reads off one revision: its version, state and the
// count of its statements applied out of its total.
type progress struct {
	Version string
	State   protocol.HistoryRevision_State
	Applied uint64
	Total   uint64
}

// progress reads the fixture's revisions in format through the provider's
// history service, which stores them as the host wrote them. An Atlas
// revision stores no state, so its state reads as unspecified.
func (f migrationsFixture) progress(c *qt.C, format protocol.HistoryScope_Format) []progress {
	c.Helper()
	binary, digest := providerBinary(c)
	client, session, closeSession := historySession(c, binary, digest)
	defer closeSession()
	table := map[protocol.HistoryScope_Format]string{protocol.HistoryScope_NATIVE: "schema_migrations", protocol.HistoryScope_ATLAS: "atlas_schema_revisions"}[format]
	snapshot := historyRead(c, client, &protocol.HistoryScope{SessionId: session, Schema: f.dir, Table: table, Format: format})
	read := make([]progress, 0, len(snapshot.GetRevisions()))
	for _, revision := range snapshot.GetRevisions() {
		read = append(read, progress{Version: revision.GetVersion(), State: revision.GetState(), Applied: revision.GetApplied(), Total: revision.GetTotal()})
	}
	return read
}

// failedStatement is the statement the fixture's native revision of version
// records as the one that failed.
func (f migrationsFixture) failedStatement(c *qt.C, version string) string {
	c.Helper()
	binary, digest := providerBinary(c)
	client, session, closeSession := historySession(c, binary, digest)
	defer closeSession()
	snapshot := historyRead(c, client, &protocol.HistoryScope{SessionId: session, Schema: f.dir, Table: "schema_migrations", Format: protocol.HistoryScope_NATIVE})
	for _, revision := range snapshot.GetRevisions() {
		if revision.GetVersion() == version {
			return revision.GetErrorStatement()
		}
	}
	return ""
}
