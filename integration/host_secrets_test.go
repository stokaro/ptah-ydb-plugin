//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// Secret values cannot be read back. The public path must instead preserve
// references, refuse missing inputs before writes, and reach a presence fixed
// point without resolving an input until an explicit rotation asks for it.
func TestHostSecretPresenceAndApprovedRotation(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := lockFixture(c)
	host, manifest := hostManifest(c)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	absolute := path.Join(database, directory, "pg.pw")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		listing, err := opened.Driver.Scheme().ListDirectory(cleanup, path.Join(database, directory))
		c.Assert(err, qt.IsNil)
		for _, entry := range listing.Children {
			if entry.Name == "pg.pw" {
				c.Check(execFixtureSchema(cleanup, opened.DB, "DROP SECRET `"+absolute+"`;"), qt.IsNil)
			}
		}
	})
	variable := "PTAH_SECRET_TEST_" + rand.Text()
	canary := "synthetic-secret-" + rand.Text()
	_, present := os.LookupEnv(variable)
	c.Assert(present, qt.IsFalse)
	source := filepath.Join(c.TempDir(), "schema.yaml")
	c.Assert(os.WriteFile(source, []byte(fmt.Sprintf("secrets:\n  pg.pw:\n    schema: %s\n    value_env: %s\n", directory, variable)), 0o600), qt.IsNil)
	var binding *string
	apply := func(extra ...string) (string, error) {
		c.Helper()
		ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
		defer cancel()
		args := []string{"schema", "apply", "--auto-approve", "--tx-mode", "none", "--provider-config", manifest, "--provider-engine", "ydb",
			"--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema-file", source, "--schemas", directory}
		command := exec.CommandContext(ctx, host, append(args, extra...)...)
		command.Env = os.Environ()
		if binding != nil {
			command.Env = append(command.Env, variable+"="+*binding)
		}
		output, err := command.CombinedOutput()
		c.Assert(string(output), qt.Not(qt.Contains), canary)
		return string(output), err
	}
	assertEmpty := func() {
		c.Helper()
		listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
		c.Assert(err, qt.IsNil)
		c.Assert(listing.Children, qt.HasLen, 0)
	}
	if !session.Capabilities["secrets"] {
		_, err := apply("--dry-run")
		c.Assert(err, qt.IsNotNil)
		assertEmpty()
		return
	}
	output, err := apply("--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, variable)
	_, err = apply()
	c.Assert(err, qt.IsNotNil)
	assertEmpty()
	binding = &canary
	output, err = apply()
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Schema apply completed successfully")
	binding = nil
	output, err = apply()
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Schema is synced, no changes to be made")
	_, err = apply("--rotate-secret", directory+"/pg.pw")
	c.Assert(err, qt.IsNotNil)
	// A present empty value is an approved input, not a missing binding.
	empty := ""
	binding = &empty
	for _, test := range []struct {
		extra []string
		fixed bool
	}{
		{extra: []string{"--rotate-secret", directory + "/pg.pw"}},
		{fixed: true},
	} {
		output, err = apply(test.extra...)
		c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
		c.Assert(strings.Contains(output, "Schema is synced, no changes to be made"), qt.Equals, test.fixed)
	}
	c.Assert(os.WriteFile(source, []byte("secrets: {}\n"), 0o600), qt.IsNil)
	output, err = apply()
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	assertEmpty()
	output, err = apply()
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Schema is synced, no changes to be made")
}
