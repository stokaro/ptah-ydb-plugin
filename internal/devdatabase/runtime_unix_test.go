//go:build unix

package devdatabase_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/devdatabase"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// fakeDocker puts a docker command on PATH that answers `docker info` and
// answers `docker context inspect` with endpoint, or fails it when endpoint is
// empty. A shell script stands in for the Docker command line, which is why
// the file builds on unix only.
func fakeDocker(c *qt.C, endpoint string) {
	c.Helper()
	directory := c.TempDir()
	inspect := "exit 1"
	if endpoint != "" {
		inspect = "echo '" + endpoint + "'"
	}
	script := "#!/bin/sh\ncase \"$1\" in\ninfo) echo 27.0.0 ;;\ncontext) " + inspect + " ;;\n*) exit 1 ;;\nesac\n"
	c.Assert(os.WriteFile(filepath.Join(directory, "docker"), []byte(script), 0o755), qt.IsNil) // #nosec G306 -- the fake command has to be executable
	c.Setenv("PATH", directory)
}

// A runtime on another machine, and one that cannot say where it runs, are
// refused: local-ydb takes every connection without a credential, so a daemon
// elsewhere would publish it to whoever reaches that machine. Ported from the
// host's TestProvisionYDB_FailurePath.
func TestDockerCLIAvailable_RefusesARuntimeItCannotPlace(t *testing.T) {
	for _, test := range []struct {
		name, host, endpoint, reason string
	}{
		{name: "DOCKER_HOST on another machine", host: "ssh://buster@remote-dev",
			reason: `docker://ydb starts ydbplatform/local-ydb:26.2.1.14, which takes every connection without a credential, and the container runtime runs on remote-dev, .*`},
		{name: "a context on another machine", endpoint: "tcp://192.0.2.10:2375",
			reason: `docker://ydb starts ydbplatform/local-ydb:26.2.1.14, which takes every connection without a credential, and the container runtime runs on 192.0.2.10, .*`},
		{name: "a runtime that cannot say where it runs",
			reason: `docker://ydb starts a server that takes connections without a credential, and the container runtime cannot say whether it runs on this machine`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fakeDocker(c, test.endpoint)
			c.Setenv("DOCKER_HOST", test.host)

			err := devdatabase.DockerCLI{RunLabel: "test"}.Available(c.Context(), "ydbplatform/local-ydb:26.2.1.14")

			var refusal *devdatabase.Refusal
			c.Assert(err, qt.ErrorAs, &refusal)
			c.Assert(refusal.Unavailable, qt.IsFalse)
			c.Assert(refusal.Reason, qt.Matches, test.reason)
		})
	}
}

// A runtime on this machine is available: a local socket, and a loopback
// address. The control for the refusals above.
func TestDockerCLIAvailable_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, host, endpoint string }{
		{name: "the default socket", endpoint: "unix:///var/run/docker.sock"},
		{name: "a loopback DOCKER_HOST", host: "tcp://127.0.0.1:2375"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fakeDocker(c, test.endpoint)
			c.Setenv("DOCKER_HOST", test.host)

			err := devdatabase.DockerCLI{RunLabel: "test"}.Available(c.Context(), "ydbplatform/local-ydb:26.2.1.14")

			c.Assert(err, qt.IsNil)
		})
	}
}
