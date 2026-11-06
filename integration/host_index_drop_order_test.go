//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// unitsNaming are the units of an apply's report, as appliedUnits lists
// them, that name one of the objects given, in plan order, each without the
// prefix and cut at its first colon.
func unitsNaming(output string, names ...string) []string {
	var named []string
	for line := range strings.SplitSeq(output, "\n") {
		unit, found := strings.CutPrefix(line, unitPrefix)
		if !found {
			continue
		}
		head, _, _ := strings.Cut(unit, ":")
		for _, name := range names {
			if strings.Contains(head, name) {
				named = append(named, head)
				break
			}
		}
	}
	return named
}

// `schema apply` creates a new table before it drops an index of a table it
// keeps, as the built-in YDB engine at host 74de0b65b planned it on 25.1.4.7
// and 26.2.1.14, and the server then holds the new table and the kept one
// without the index. Through the provider the host dropped every index first.
func TestHostCreatesATableBeforeItDropsAnIndex_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	kept, created := path.Join(database, directory, "p"), path.Join(database, directory, "n")
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{kept, created} {
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE `"+table+"`"), qt.IsNil, qt.Commentf("%s", table))
		}
	})
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+kept+"` (id Uint64 NOT NULL, a Utf8, PRIMARY KEY (id), INDEX p_a GLOBAL SYNC ON (a))"), qt.IsNil)
	declared := "tables:\n  n:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true}\n" +
		"  p:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true}\n      a: {type: Utf8}\n"

	output, err := hostApplySource(c, directory, "yaml", declared)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(unitsNaming(output, directory+".n", directory+".p"), qt.DeepEquals, []string{
		"github.com/stokaro/ptah-ydb-plugin/row-table-create-operation " + directory + ".n",
		"github.com/stokaro/ptah-ydb-plugin/row-table-alter-operation " + directory + ".p",
	}, qt.Commentf("%s", output))
	c.Assert(heldIndexNames(c, opened, kept), qt.HasLen, 0)
	_, found := describedChangefeeds(c, opened, created)
	c.Assert(found, qt.IsTrue)
}
