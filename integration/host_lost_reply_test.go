//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// replyCutBuild holds the reply-cut harness, built once per test binary.
var replyCutBuild struct {
	sync.Mutex
	done                    bool
	binary, digest, failure string
}

// replyCutBinary builds internal/cmd/replycut and returns its path and
// digest.
func replyCutBinary(c *qt.C) (binary, digest string) {
	c.Helper()
	replyCutBuild.Lock()
	defer replyCutBuild.Unlock()
	if !replyCutBuild.done {
		replyCutBuild.binary, replyCutBuild.digest, replyCutBuild.failure = buildReplyCut()
		replyCutBuild.done = true
	}
	c.Assert(replyCutBuild.failure, qt.Equals, "")
	return replyCutBuild.binary, replyCutBuild.digest
}

// buildReplyCut compiles the harness, or says why it could not.
func buildReplyCut() (binary, digest, failure string) {
	dir, err := os.MkdirTemp("", "ptah-plugin-ydb-replycut-")
	if err != nil {
		return "", "", err.Error()
	}
	binary = filepath.Join(dir, "replycut")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if output, err := testCommand(ctx, "go", "build", "-o", binary, "../internal/cmd/replycut").CombinedOutput(); err != nil {
		return "", "", fmt.Sprintf("build replycut: %v: %s", err, output)
	}
	contents, err := os.ReadFile(binary)
	if err != nil {
		return "", "", err.Error()
	}
	sum := sha256.Sum256(contents)
	return binary, hex.EncodeToString(sum[:]), ""
}

// replyCutManifest is providerManifest whose executable is the reply-cut
// harness in front of this provider: it loses the reply to the COMMIT of the
// transaction a statement holding mark runs in, and the first reads history
// reads after that, and records each lost reply in the file log.
func replyCutManifest(c *qt.C, mark string, reads int, log string) string {
	c.Helper()
	provider, providerDigest := providerBinary(c)
	harness, harnessDigest := replyCutBinary(c)
	environment := []string{"REPLYCUT_PROVIDER=" + provider, "REPLYCUT_PROVIDER_SHA256=" + providerDigest, "REPLYCUT_MARK=" + mark,
		"REPLYCUT_READS=" + strconv.Itoa(reads), "REPLYCUT_LOG=" + log}
	configuration := map[string]any{"format_version": 2, "providers": []any{map[string]any{
		"executable": harness, "sha256": harnessDigest, "provider_id": schema.ProviderID, "engines": []any{map[string]any{"id": "ydb", "aliases": []string{"ydbs"}}},
		"environment": environment,
	}}}
	encoded, err := json.Marshal(configuration)
	c.Assert(err, qt.IsNil)
	manifest := filepath.Join(c.TempDir(), "replycut.json")
	c.Assert(os.WriteFile(manifest, encoded, 0o600), qt.IsNil)
	return manifest
}

// lostReplyMigrations writes the migration whose second statement, a data
// query, commits with its checkpoint, between two table creations.
func lostReplyMigrations(c *qt.C, f migrationsFixture) string {
	c.Helper()
	return writeFiles(c, c.TempDir(), map[string]string{
		"0000000001_l.up.sql": "CREATE TABLE `" + f.table("l") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
			"INSERT INTO `" + f.table("l") + "` (id) VALUES (1l);\n" +
			"CREATE TABLE `" + f.table("m") + "` (id Int64 NOT NULL, PRIMARY KEY (id));\n",
		"0000000001_l.down.sql": "DROP TABLE `" + f.table("m") + "`;\nDROP TABLE `" + f.table("l") + "`;\n",
	})
}

// A run whose reply to the commit of a data query is lost after the provider
// committed it reads the revision back, finds the query's checkpoint, and
// goes on: the INSERT runs once, the last statement runs, and the revision is
// applied. The reply is lost by the reply-cut harness in front of this
// provider (internal/cmd/replycut). The host's fixture test pins the rule
// (provider_migration_units_e2e_test.go, "the read-back settles it").
func TestHostReadsBackACommitWhoseReplyWasLost_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	migrations := lostReplyMigrations(c, f)
	log := filepath.Join(c.TempDir(), "replycut.log")
	manifest := replyCutManifest(c, "INSERT INTO", 0, log)

	output, err := runHost(c, pinnedHost(c), append(append([]string{"migrations", "up"}, f.target(migrations)...),
		"--provider-config", manifest, "--provider-engine", "ydb")...)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	lost, readErr := os.ReadFile(log)
	c.Assert(readErr, qt.IsNil)
	c.Assert(string(lost), qt.Equals, "lost the reply to the COMMIT of a transaction that ran INSERT INTO\n")
	c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", applied, 3, 3}})
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("l")+"`"), qt.Equals, int64(1))
	c.Assert(f.names(c), qt.Contains, "m")
}

// Ported from the host's TestYDBMigrator_FailureKeepsTheProgressTheRowRecords
// (integration/dbschema/ydb): the revision row records what a body
// committed, and a failure never records less. Here the reply to the commit
// of the data query is lost after the provider committed it, with its
// checkpoint, and so is the history read that follows, so the host cannot
// read back whether the commit landed and the run fails. The row then reads
// failed at 2 of 3, and the run that resumes it does not run the INSERT
// again: it runs the last statement alone. The host's test placed the lost
// answer with a statement interceptor of the built-in engine; through a
// provider the reply-cut harness in front of it does (internal/cmd/replycut).
func TestHostFailureKeepsTheProgressTheRowRecords_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	f := newMigrationsFixture(c)
	migrations := lostReplyMigrations(c, f)
	log := filepath.Join(c.TempDir(), "replycut.log")
	manifest := replyCutManifest(c, "INSERT INTO", 1, log)

	lost, lostErr := runHost(c, pinnedHost(c), append(append([]string{"migrations", "up"}, f.target(migrations)...),
		"--provider-config", manifest, "--provider-engine", "ydb")...)
	recorded := f.progress(c, protocol.HistoryScope_NATIVE)
	resumed, resumeErr := hostCommand(c, append(append([]string{"migrations", "up"}, f.target(migrations)...), "--allow-dirty")...)

	c.Assert(lostErr, qt.IsNotNil, qt.Commentf("%s", lost))
	record, readErr := os.ReadFile(log)
	c.Assert(readErr, qt.IsNil)
	c.Assert(string(record), qt.Equals, "lost the reply to the COMMIT of a transaction that ran INSERT INTO\nlost the reply to a history read\n")
	c.Assert(lost, qt.Contains, "provider finish transaction failed (Unavailable)\nread back whether the query committed: provider read history failed (Unavailable)")
	c.Assert(recorded, qt.DeepEquals, []progress{{"1", failed, 2, 3}})
	c.Assert(resumeErr, qt.IsNil, qt.Commentf("%s", resumed))
	c.Assert(f.progress(c, protocol.HistoryScope_NATIVE), qt.DeepEquals, []progress{{"1", applied, 3, 3}})
	c.Assert(f.count(c, "SELECT COUNT(*) FROM `"+f.table("l")+"`"), qt.Equals, int64(1))
	c.Assert(f.names(c), qt.Contains, "m")
}
