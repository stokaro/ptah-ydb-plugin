//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	qt "github.com/frankban/quicktest"
)

// compatBinaryVariable names the pinned host's ptah-compat, the Atlas-
// compatible binary, which CI builds from the same checkout as the host
// binary PTAH_HOST_TEST_BIN names.
const compatBinaryVariable = "PTAH_HOST_COMPAT_TEST_BIN"

// builtInClaim is how ptah-compat refuses this provider's registration while
// a YDB engine built into the host claims the ydb target. The compatibility
// surface registers a provider beside the built-in engines and never in
// place of one, so such a host cannot hand a ydb:// URL to any provider
// (stokaro/ptah#4204, gap G71); a host without the built-in engine can
// (stokaro/ptah#5298).
const builtInClaim = `target name "ydb" is claimed by "ptah.run/ydb"`

// compatReach holds what each ptah-compat binary answered the probe, by path:
// the empty string for one that hands a ydb:// URL to this provider, and
// otherwise why the compatibility tests skip there. The probe starts the
// provider once per binary rather than once per test.
var compatReach struct {
	sync.Mutex
	answers map[string]string
}

// hostCompat returns the pinned host's ptah-compat and a manifest that selects
// this provider for ydb. ptah-compat reads the manifest from
// PTAH_PROVIDER_CONFIG, because it registers no flag the community binary
// lacks. The test is skipped without the binary, and on a host whose
// ptah-compat keeps a built-in YDB engine, which is decided by asking the
// binary rather than by its version.
func hostCompat(c *qt.C) (binary, manifest string) {
	c.Helper()
	binary = os.Getenv(compatBinaryVariable)
	if binary == "" {
		c.Skipf("%s is required for the host's compatibility surface", compatBinaryVariable)
	}
	manifest = providerManifest(c)
	compatReach.Lock()
	defer compatReach.Unlock()
	answer, probed := compatReach.answers[binary]
	if !probed {
		answer = probeCompat(c, binary, manifest)
		if compatReach.answers == nil {
			compatReach.answers = map[string]string{}
		}
		compatReach.answers[binary] = answer
	}
	if answer != "" {
		c.Skip(answer)
	}
	return binary, manifest
}

// probeCompat inspects a directory the test database does not hold through
// ptah-compat, which starts the provider for the URL and reads nothing. It
// returns the empty string when the run succeeds, and the reason to skip when
// the host's built-in YDB engine refuses the provider. Any other answer fails
// the test, so a host that should reach the provider and does not is a
// failure rather than a skip.
func probeCompat(c *qt.C, binary, manifest string) string {
	c.Helper()
	missing := "ptah4204_compat_probe_" + strings.ToLower(rand.Text())[:12]
	stdout, stderr, exit := runCompatBinary(c, binary, manifest, nil, "schema", "inspect", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", missing)
	if exit == 0 {
		return ""
	}
	if strings.Contains(stderr, builtInClaim) {
		return fmt.Sprintf("ptah-compat keeps a built-in YDB engine that claims ydb before this provider can (%s); "+
			"the compatibility tests need a host without it, stokaro/ptah#5298", strings.TrimSpace(stderr))
	}
	c.Fatalf("ptah-compat could not reach the provider: exit %d\nstdout:\n%s\nstderr:\n%s", exit, stdout, stderr)
	return ""
}

// runCompat runs the pinned host's ptah-compat with this provider selected
// for ydb and the variables in env added to its environment. It returns the
// two streams apart, as schema inspect writes the document to standard output
// and its notes to standard error, and the exit status.
func runCompat(c *qt.C, env []string, args ...string) (stdout, stderr string, exit int) {
	c.Helper()
	binary, manifest := hostCompat(c)
	return runCompatBinary(c, binary, manifest, env, args...)
}

func runCompatBinary(c *qt.C, binary, manifest string, env []string, args ...string) (stdout, stderr string, exit int) {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()
	cmd := compatCommand(ctx, binary, manifest, env, args...)
	var out, diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostics
	err := cmd.Run()
	var exited *exec.ExitError
	switch {
	case err == nil:
		return out.String(), diagnostics.String(), 0
	case errors.As(err, &exited):
		return out.String(), diagnostics.String(), exited.ExitCode()
	}
	c.Fatalf("run ptah-compat: %v\n%s", err, diagnostics.String())
	return "", "", -1
}

// compatCommand is ptah-compat with args, PTAH_PROVIDER_CONFIG naming
// manifest and the variables in env added, for a test that drives the
// process itself, such as one that answers a prompt. A variable in env
// replaces one the test process has.
func compatCommand(ctx context.Context, binary, manifest string, env []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(append(os.Environ(), "PTAH_PROVIDER_CONFIG="+manifest), env...)
	return cmd
}

// compatFile writes body to name in dir and returns the path with forward
// slashes, so it can follow file:// in a URL ptah-compat takes.
func compatFile(c *qt.C, dir, name, body string) string {
	c.Helper()
	file := filepath.Join(dir, name)
	c.Assert(os.MkdirAll(filepath.Dir(file), 0o750), qt.IsNil)
	c.Assert(os.WriteFile(file, []byte(body), 0o600), qt.IsNil)
	return filepath.ToSlash(file)
}
