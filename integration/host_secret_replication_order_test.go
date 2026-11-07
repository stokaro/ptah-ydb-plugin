//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The former host TestGenerateMigrationAST_SecretsPrecedeReplications requires
// a secret to exist before the async replication that reads it starts. The
// published host must preserve this order in a plan that creates both.
func TestHostCreatesReplicationSecretBeforeItsReader(t *testing.T) {
	variable := secretVariable(t, secretValue())
	c := testcheck.New(t)
	h := newHostReplication(c)
	skipWithout(c, h.session, secrets.Capability)
	c.Assert(execFixtureSchema(c.Context(), h.opened.DB,
		"CREATE TABLE `"+h.absolute("src")+"` (id Int64 NOT NULL, note Utf8, PRIMARY KEY (id))"), qt.IsNil)

	mirror := strings.Replace(h.mirror, "\n    items:",
		"\n    token_secret_path: "+path.Join(h.directory, "token")+"\n    items:", 1)
	c.Assert(mirror, qt.Contains, "token_secret_path: "+path.Join(h.directory, "token"))
	source := h.tables("src") + "secrets:\n  token:\n    schema: " + h.directory +
		"\n    value_env: " + variable + "\n" + mirror

	c.Assert(hostPlanned(c, h.directory, "yaml", source), qt.DeepEquals, []string{
		"secret-operation " + h.directory + ".token",
		"async-replication-operation " + h.directory + ".mirror",
	})
}
