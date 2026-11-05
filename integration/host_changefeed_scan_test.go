//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// Ported from the host's TestYDBChangefeeds_WaitsOutAnInitialScan: two
// changefeeds are added to a table of 300000 rows, the first with an initial
// scan. The scan holds the table's path until it has read every row, and YDB
// refuses the second ALTER TABLE in the meantime with OVERLOADED (`path ...
// has been locked by tx`). The refusal changed nothing, so the provider runs
// the statement again once the scan is done: both changefeeds are added, and
// nothing is left to plan. Run once, the second is never added and the run
// fails halfway.
func TestHostWaitsOutAnInitialScan_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "events")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	table := "tables:\n  events:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      payload: {type: Utf8}\n"
	declared := table + "    changefeeds:\n      audit:\n        mode: NEW_AND_OLD_IMAGES\n        format: JSON\n        initial_scan: true\n" +
		"      keys:\n        mode: KEYS_ONLY\n        format: JSON\n"
	hostRun(c, directory, "the table", table, `(?s).*Schema apply completed successfully.*`)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` SELECT x AS id, CAST(CAST(x AS String) AS Utf8) AS payload "+
		"FROM (SELECT ListFromRange(0ul, 300000ul) AS x) FLATTEN LIST BY x"), qt.IsNil)

	output, err := hostApplySource(c, directory, "yaml", declared)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(unitCount(output, "github.com/stokaro/ptah-ydb-plugin/changefeed-operation"), qt.Equals, 1, qt.Commentf("%s", output))
	shapes, found := describedChangefeeds(c, opened, absolute)
	c.Assert(found, qt.IsTrue)
	c.Assert(shapes, qt.DeepEquals, map[string]feedShape{
		"audit": {Mode: options.ChangefeedModeNewAndOldImages, Retention: 24 * time.Hour},
		"keys":  {Mode: options.ChangefeedModeKeysOnly, Retention: 24 * time.Hour},
	})
	hostRun(c, directory, "a second run", declared, `(?s).*Schema is synced, no changes to be made.*`)
}
