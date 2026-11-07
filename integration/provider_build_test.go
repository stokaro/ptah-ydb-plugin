//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

// providerBuild is the provider binary this test process compiled with the
// race detector, in a directory TestMain removes, or the reason it could not
// be built. A build per call would compile, link and hash the provider again
// for every host command the suite runs, and a failed build would be retried
// by every test after it.
var providerBuild struct {
	sync.Mutex
	done                         bool
	dir, binary, digest, failure string
}

func TestMain(m *testing.M) {
	code := m.Run()
	providerBuild.Lock()
	if providerBuild.dir != "" {
		_ = os.RemoveAll(providerBuild.dir)
	}
	providerBuild.Unlock()
	os.Exit(code)
}

// providerBinary returns the provider built with the race detector and its
// digest, for the tests that start it directly and the host-driven tests
// alike. PTAH_YDB_PROVIDER_BIN names a binary built elsewhere instead, such
// as the one a release archives, so the release workflow tests the provider
// it publishes rather than a build of its own.
func providerBinary(c *qt.C) (binary, digest string) {
	c.Helper()
	providerBuild.Lock()
	defer providerBuild.Unlock()
	if !providerBuild.done {
		providerBuild.failure = buildProvider()
		providerBuild.done = true
	}
	c.Assert(providerBuild.failure, qt.Equals, "")
	return providerBuild.binary, providerBuild.digest
}

// buildProvider compiles the provider and returns why it could not, or the
// empty string. The caller holds providerBuild.
func buildProvider() string {
	if prebuilt := os.Getenv("PTAH_YDB_PROVIDER_BIN"); prebuilt != "" {
		return usePrebuiltProvider(prebuilt)
	}
	dir, err := os.MkdirTemp("", "ptah-plugin-ydb-build-")
	if err != nil {
		return err.Error()
	}
	providerBuild.dir = dir
	name := "ptah-plugin-ydb"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(dir, name)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	output, err := testCommand(ctx, "go", "build", "-race", "-o", binary, "../cmd/ptah-plugin-ydb").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("build provider: %v: %s", err, output)
	}
	contents, err := os.ReadFile(binary)
	if err != nil {
		return err.Error()
	}
	digest := sha256.Sum256(contents)
	providerBuild.binary, providerBuild.digest = binary, hex.EncodeToString(digest[:])
	return ""
}

// usePrebuiltProvider takes the binary at path as the provider, with its
// digest, and returns why it cannot, or the empty string. The caller holds
// providerBuild.
func usePrebuiltProvider(path string) string {
	contents, err := os.ReadFile(path) // #nosec G703 -- the release workflow names the binary it built, in its own workspace
	if err != nil {
		return fmt.Sprintf("read the prebuilt provider: %v", err)
	}
	digest := sha256.Sum256(contents)
	providerBuild.binary, providerBuild.digest = path, hex.EncodeToString(digest[:])
	return ""
}
