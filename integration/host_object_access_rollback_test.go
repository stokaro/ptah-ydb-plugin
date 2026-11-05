//go:build integration

package integration_test

import (
	"path"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBObjectAccess_Rollback: `migrations generate`
// writes the creation of a coordination node and, where the line has
// secrets, a secret, with the user's grant on each, and its reverse;
// `migrations up` applies it and leaves nothing to plan, and `migrations down`
// drops every object, and every entry the forward migration made goes with
// its object.
//
// The host's fixture holds a topic too. This provider writes no migration
// file for a topic: its topic operation has no YQL text form, and the
// generation refuses it, so the topic is left out.
func TestHostRollsBackObjectAccess_HappyPath(t *testing.T) {
	t.Setenv(objectAccessSecretEnv, "object-access-v1")
	c := testcheck.New(t)
	f := objectAccessFixture(c)
	emptyAtCleanup(c, f.opened, path.Join(f.database, f.directory))
	objects := slices.DeleteFunc(slices.Clone(f.objects), func(object accessObject) bool { return object.document == "topics" })
	declared := f.document(objects, "")

	migrations, _, down := hostGenerate(c, f.directory, "yaml", declared)
	applied, upErr := hostMigrate(c, f.directory, migrations, "migrations", "up")
	granted := f.grants(c)
	settled := hostPlanned(c, f.directory, "yaml", declared)
	rolledBack, downErr := hostMigrate(c, f.directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(granted, qt.DeepEquals, objectAccessExpected(f.directory, f.names, objects, ""))
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(f.grants(c), qt.HasLen, 0)
	c.Assert(objectsIn(heldEntries(c, f.opened, path.Join(f.database, f.directory))), qt.HasLen, 0)
}
