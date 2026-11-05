//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBIntrospect_ModelsPlanNothingAgainstTheirDatabase:
// `ptah introspect` over the round-trip schema, which carries every type the
// line takes, a literal default of each kind, a Serial key, a composite key,
// unique, asynchronous and covering indexes, a nested directory and names
// that need escaping, imports its four tables. The Go models it writes,
// applied back over the directories they came from, plan nothing, and each
// field takes the Go type the host's embedded engine gives its column.
func TestHostIntrospectsTheRoundTripSchema_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	c.Cleanup(func() {
		// emptyTree quotes a name as it is, and a backquote in the name
		// ends the quoted identifier, so this table goes first, escaped.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "tick\\`name.with.dot")+"`"), qt.IsNil)
	})
	selectors := directory + "," + directory + "/archive"
	created, err := hostApplySelected(c, selectors, "yaml", roundTripSchema(directory, targetCapabilities(c)))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(created, qt.Matches, `(?s).*Schema apply completed successfully.*`)
	out := c.TempDir()

	imported, _ := hostExport(c, "introspect", "--schemas", selectors, "--out", out, "--single-file")
	models, err := os.ReadFile(filepath.Join(out, "schema.go"))
	c.Assert(err, qt.IsNil)
	replanned, replanErr := hostApplySelected(c, selectors, "go", string(models))

	c.Assert(imported, qt.Contains, "Imported 4 table(s)")
	c.Assert(replanErr, qt.IsNil, qt.Commentf("%s\n%s", models, replanned))
	c.Assert(replanned, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", models, replanned))
	for _, want := range []string{
		"\tId int64\n", "\tEmail string\n", "\tTiny *int8\n", "\tU64 *uint64\n", "\tRatio *float32\n",
		"\tAvatar []byte\n", "\tDoc []byte\n", "\tToken *string\n", "\tCreatedAt *time.Time\n", "\tNIv *time.Duration\n",
	} {
		c.Assert(string(models), qt.Contains, want)
	}
}
