//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A busy lock says why to a client that lists errors.reasons, and the same
// refusal is its outcome alone to one that does not. The lock is held on the
// server by another provider process.
func TestBusyLockGivesItsReasonSubprocess(t *testing.T) {
	for _, test := range []struct {
		name         string
		hostFeatures []string
		want         string
	}{
		{"a client that lists errors.reasons", []string{provider.ErrorReasonsFeature}, "provider lock outcome is BUSY: another YDB session holds the lock"},
		{"a client that does not list it", nil, "provider lock outcome is BUSY"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			lockFixture(c)
			holder, waiter := startProviderFor(c), startProviderFor(c, test.hostFeatures...)
			held, err := holder.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(holder.CloseSession(context.Background(), held.Id), qt.IsNil) })
			waiting, err := waiter.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(waiter.CloseSession(context.Background(), waiting.Id), qt.IsNil) })
			acquireNativeLock(c, holder, nativeLockRequest(held.Id, "held", "reasons"))

			result, err := waiter.TryLock(c.Context(), nativeLockRequest(waiting.Id, "waiting", "reasons"))

			c.Assert(err, qt.ErrorIs, provider.ErrLockBusy)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(result.GetOutcome(), qt.Equals, protocol.LockResult_BUSY)
		})
	}
}

// hostNegotiates reports whether the selected host negotiates feature with
// this provider, from the features `providers check` reports. A host that
// reports none negotiates nothing this test can rely on.
func hostNegotiates(c *qt.C, host, manifest, feature string) bool {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, host, "providers", "check", "--provider-config", manifest, "--provider-engine", "ydb", "--json").Output()
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	var report struct {
		Provider struct {
			Features []string `json:"features"`
		} `json:"provider"`
	}
	c.Assert(json.Unmarshal(output, &report), qt.IsNil)
	return slices.Contains(report.Provider.Features, feature)
}

// A host that negotiates errors.reasons prints the provider's reason after
// the code: here why the provider refuses to compare a streaming query on a
// target where none is established. Without the monitoring endpoint a session
// takes each release line's default capabilities, under which no streaming
// query is established, whatever the server's flags. The refused run writes
// nothing.
func TestHostPrintsTheProvidersReasonSubprocess(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	if !hostNegotiates(c, host, manifest, provider.ErrorReasonsFeature) {
		c.Skip("the selected host did not negotiate errors.reasons")
	}
	opened, directory, database := lockFixture(c)
	source := filepath.Join(c.TempDir(), "schema.sql")
	c.Assert(os.WriteFile(source, []byte(streaming.Create(directory, "refused", streaming.Spec{Text: "SELECT 1;"})), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, host, "schema", "apply", "--auto-approve", "--tx-mode", "none", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", withoutMonitoring(c, os.Getenv("PTAH_YDB_TEST_URL")), "--schemas", directory, "--schema-file", source)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output

	err := command.Run()

	c.Assert(err, qt.IsNotNil)
	c.Assert(output.String(), qt.Contains, "provider compare models failed (InvalidArgument): streaming query capability was not established")
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	c.Assert(listing.Children, qt.HasLen, 0)
}
