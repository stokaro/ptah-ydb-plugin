//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
)

func providerBinary(c *qt.C) (string, string) {
	c.Helper()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	binary := filepath.Join(c.TempDir(), "ptah-plugin-ydb"+suffix)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-race", "-o", binary, "../cmd/ptah-plugin-ydb")
	output, err := cmd.CombinedOutput()
	c.Assert(err, qt.IsNil, qt.Commentf("build provider: %s", output))
	bytes, err := os.ReadFile(binary)
	c.Assert(err, qt.IsNil)
	digest := sha256.Sum256(bytes)
	return binary, hex.EncodeToString(digest[:])
}

func startProvider(c *qt.C, binary, digest string) *providerruntime.Client {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{
		Executable: binary, SHA256: digest, ProviderID: "github.com/stokaro/ptah-ydb-plugin",
		HostFeatures: []string{provider.PlanningLifecycleFeature},
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	return client
}

func TestNativeSessionsSubprocess(t *testing.T) {
	c := qt.New(t)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	c.Assert(address, qt.Not(qt.Equals), "", qt.Commentf("PTAH_YDB_TEST_URL must select a live YDB test database"))
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	first, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Assert(first.GetDatabase(), qt.Equals, "/local")
	c.Assert(first.GetServerVersion(), qt.Not(qt.Equals), "")
	c.Logf("native YDB version: %s", first.GetServerVersion())
	second, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Assert(second.GetId(), qt.Not(qt.Equals), first.GetId())
	c.Assert(second.GetServerVersion(), qt.Equals, first.GetServerVersion())
	c.Assert(client.CloseSession(ctx, first.GetId()), qt.IsNil)
	c.Assert(client.CloseSession(ctx, first.GetId()), qt.IsNil)
	// Closing a handle does not prevent another session from opening.
	third, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Assert(client.CloseSession(ctx, second.GetId()), qt.IsNil)
	other := startProvider(c, binary, digest)
	c.Assert(other.CloseSession(ctx, first.GetId()), qt.ErrorMatches, `.*\(NotFound\)`)
	c.Assert(client.CloseSession(ctx, "missing"), qt.ErrorMatches, `.*\(NotFound\)`)
	_, err = client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: "ydb://alice:test-secret@host/local?token=other"})
	c.Assert(err, qt.IsNotNil)
	c.Assert(err.Error(), qt.Not(qt.Contains), "test-secret")
	c.Assert(err.Error(), qt.Not(qt.Contains), "other")
	// A closed session cannot inspect through its previously opened driver.
	_, err = client.Inspect(ctx, &protocol.InspectRequest{SessionId: first.GetId()})
	c.Assert(err, qt.ErrorMatches, `.*\(FailedPrecondition\)`)
	c.Assert(client.CloseSession(ctx, third.GetId()), qt.IsNil)
	// A new process does not accept old handles even when the database is equal.
	live, err := other.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Assert(live.GetId(), qt.Not(qt.Equals), first.GetId())
	c.Assert(other.CloseSession(ctx, live.GetId()), qt.IsNil)
}

func TestNativeSessionConcurrentClose(t *testing.T) {
	c := qt.New(t)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	c.Assert(address, qt.Not(qt.Equals), "")
	service := backend.New()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	session, err := service.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	// The backend releases native resources even after the request was canceled.
	canceled, stop := context.WithCancel(ctx)
	stop()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = service.Close(canceled, &protocol.CloseRequest{SessionId: session.GetId()}) })
	}
	wg.Wait()
	c.Assert(errs, qt.DeepEquals, make([]error, 8))
	refused, err := service.Open(canceled, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(refused, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
}

func TestNativeConnectionReleasesBothOwners(t *testing.T) {
	c := qt.New(t)
	address, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	_, err = opened.Driver.Scheme().ListDirectory(ctx, address.Database())
	c.Assert(err, qt.IsNil)
	other, err := connection.Open(ctx, address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(other.Close(), qt.IsNil) })
	c.Assert(opened.Close(), qt.IsNil)
	var version string
	c.Assert(other.DB.QueryRowContext(ctx, "SELECT Version()").Scan(&version), qt.IsNil)
	c.Assert(version, qt.Not(qt.Equals), "")
	c.Assert(opened.DB.PingContext(ctx), qt.IsNotNil)
	_, err = opened.Driver.Scheme().ListDirectory(ctx, address.Database())
	c.Assert(err, qt.IsNotNil)
	c.Assert(opened.Close(), qt.IsNil)
}
