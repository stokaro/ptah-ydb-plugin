//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBAccessControl_Rollback: `migrations generate`
// writes the creation of principals, memberships and grants and its reverse,
// `migrations up` applies it and leaves nothing to plan, and `migrations down`
// takes every principal, membership and grant away. No entry of a principal
// is left on any object of the database: the reverse revokes a principal's
// entries before it drops the principal, because DROP USER leaves them
// behind for the next principal of that name.
//
// The table the grants name is applied before the generation, as
// TestHostAppliesAccessControl_HappyPath applies it.
func TestHostRollsBackAccessControl_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client, session := accessSession(c)
	names := newAccessNames()
	removeAccess(c, opened, database, names)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	declared := accessDeclaration(directory, names, []string{"DATA-READERS", names.group}, false, "SELECT ROW")
	hostRun(c, directory, "the table", accessTable(directory), `(?s).*Schema apply completed successfully.*`)

	migrations, up, down := hostGenerate(c, directory, "yaml", declared)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	settled := hostPlanned(c, directory, "yaml", declared)
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	roles, memberships, grants := principalsOf(c, client, session, directory, names)
	var entries uint64
	c.Assert(opened.DB.QueryRowContext(c.Context(), "SELECT COUNT(*) FROM `"+database+"/.sys/auth_permissions` WHERE Sid IN ('"+
		strings.Join(names.all(), "'u, '")+"'u)").Scan(&entries), qt.IsNil)

	c.Assert(up, qt.Contains, "CREATE GROUP `"+names.group+"`")
	c.Assert(down, qt.Contains, "DROP USER `"+names.user+"`")
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(roles, qt.HasLen, 0)
	c.Assert(memberships, qt.HasLen, 0)
	c.Assert(grants, qt.HasLen, 0)
	c.Assert(entries, qt.Equals, uint64(0))
}
