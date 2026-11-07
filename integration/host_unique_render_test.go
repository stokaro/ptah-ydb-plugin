//go:build integration

package integration_test

import (
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The source renderer receives a complete table before comparison turns its
// UNIQUE declarations into indexes. This covers the creation cases of the
// former host's TestRender_UniqueConstraint_HappyPath through the host binary.
func TestHostRendersUniqueDeclarations_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	source := strings.Replace(uniqueAccounts("shop"), "//ptah:schema:table", "//ptah:schema:constraint type=\"UNIQUE\" columns=\"login\"\n//ptah:schema:table", 1)
	dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"accounts.go": source})
	for _, version := range []string{"25.1.4.7", "26.2.1.14"} {
		t.Run(version, func(t *testing.T) {
			c := testcheck.New(t)
			out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb", "--server-version", version)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			for _, statement := range []string{
				"INDEX `uq_accounts_tenant_login` GLOBAL UNIQUE SYNC ON (`tenant`, `login`)",
				"INDEX `accounts_login_key` GLOBAL UNIQUE SYNC ON (`login`)",
				"INDEX `accounts_email_key` GLOBAL UNIQUE SYNC ON (`email`)",
			} {
				c.Assert(out, qt.Contains, statement)
			}
			c.Assert(out, qt.Not(qt.Contains), "accounts_id_key")
		})
	}
}
