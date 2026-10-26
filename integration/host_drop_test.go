//go:build integration

package integration_test

import (
	"context"
	"path"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
)

// The host creates a table through the provider and drops it once the source
// no longer declares it, and a later run finds nothing to do.
func TestHostDropsATableTheSourceLeavesOut_HappyPath(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "items")+"`"), qt.IsNil)
	})
	output, err := hostApply(c, directory, hostItems(directory, ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(slices.Contains(childNames(c, opened, path.Join(database, directory)), "items"), qt.IsTrue)

	output, err = hostApply(c, directory, "package entities\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*table-drop-operation.*Schema apply completed successfully.*`)
	c.Assert(slices.Contains(childNames(c, opened, path.Join(database, directory)), "items"), qt.IsFalse)
}
