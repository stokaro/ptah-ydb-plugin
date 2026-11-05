//go:build integration

package integration_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/devdatabase"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// requireDocker skips a test that starts a dev database when no Docker daemon
// answers on the machine that runs it, where the provider would run docker.
func requireDocker(c *qt.C) {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		c.Skipf("a Docker daemon is required to start a dev database: %v: %s", err, output)
	}
}

// dockerManifest is a provider manifest whose provider starts with the
// variables the docker command needs, as an operator's manifest passes them:
// the provider sees nothing of the host's environment it is not given.
func dockerManifest(c *qt.C) string {
	c.Helper()
	var environment []string
	for _, name := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	return providerManifestWith(c, environment)
}

// devContainers lists the containers the provider started, by the label it
// gives each one, as their names and states.
func devContainers(c *qt.C) []string {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", "ps", "--all", "--filter", "label="+devdatabase.ContainerLabel,
		"--format", "{{.Names}} {{.State}}").Output()
	c.Assert(err, qt.IsNil)
	return strings.FieldsFunc(string(output), func(r rune) bool { return r == '\n' })
}

// devDockerMigrations is the host's migration directory for a dev database:
// a table and a user, a statement whose effect reaches the whole database.
var devDockerMigrations = map[string]string{
	"0000000001_users.up.sql": "CREATE TABLE `app/users` (`id` Int64 NOT NULL, `name` Utf8, PRIMARY KEY (`id`));\n" +
		"CREATE USER ptahdevreader PASSWORD 'secret';\n",
	"0000000001_users.down.sql": "DROP TABLE `app/users`;\nDROP USER ptahdevreader;\n",
}

// hashedMigrations writes devDockerMigrations to a directory of its own with
// the host's integrity file, and returns the directory.
func hashedMigrations(c *qt.C, host string) string {
	c.Helper()
	dir := writeFiles(c, filepath.Join(c.TempDir(), "migrations"), devDockerMigrations)
	hashed, err := runHost(c, host, "migrations", "hash", "--dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", hashed))
	return dir
}

// Ported from the host's TestDevDockerStartsYDBE2E: the host replays a
// migration directory on a `docker://ydb/<tag>[/local]` dev database the
// provider starts, on each certified line, and no container is left. The
// server is the run's own, so the replay runs a statement whose effect
// reaches the whole database, a user, which a dev realm refuses.
func TestHostValidatesMigrationsOnADockerDevDatabase_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, devURL string }{
		{name: "26.2", devURL: "docker://ydb/26.2.1.14/local"},
		{name: "25.1 with no database", devURL: "docker://ydb/25.1.4.7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			host, _ := hostManifest(c)
			requireDocker(c)
			manifest := dockerManifest(c)
			before := devContainers(c)
			dir := hashedMigrations(c, host)

			validated, err := runHost(c, host, "migrations", "validate", "--dir", dir, "--dev-url", test.devURL,
				"--provider-config", manifest, "--provider-engine", "ydb")

			c.Assert(err, qt.IsNil, qt.Commentf("%s", validated))
			c.Assert(validated, qt.Contains, "OK: migration SQL validated on dev database")
			c.Assert(devContainers(c), qt.DeepEquals, before)
		})
	}
}

// The dev URL reaches the provider: a provider whose manifest passes it no
// PATH cannot run the docker command, so it refuses to start the dev
// database, and the host reports the provider's reason. It needs no Docker
// daemon, and it starts nothing.
func TestHostValidatesMigrationsOnADockerDevDatabase_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	host, manifest := hostManifest(c)
	dir := hashedMigrations(c, host)

	validated, err := runHost(c, host, "migrations", "validate", "--dir", dir, "--dev-url", "docker://ydb/26.2.1.14/local",
		"--provider-config", manifest, "--provider-engine", "ydb")

	c.Assert(err, qt.IsNotNil)
	c.Assert(validated, qt.Contains, "provider engine ydb cannot provision a dev database now: the docker command is not on PATH")
}
