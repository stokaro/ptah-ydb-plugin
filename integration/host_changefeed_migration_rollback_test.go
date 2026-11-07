//go:build integration

package integration_test

import (
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// rollbackFeeds declares a table with the given complete changefeed set.
func rollbackFeeds(directory, feeds string) string {
	return "tables:\n  items:\n    schema: " + directory +
		"\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n    changefeeds:\n" + feeds
}

// A generated down migration drops the newly added feed and recreates the
// one the forward migration removed, including its consumer. The recreated
// feed cannot recover the messages or consumer position that its drop lost,
// and the down file records that limit. The host and server agree after both
// directions. This ports TestPlanBidirectionalSchemaDiff_ChangefeedsRollBack.
func TestHostRollsBackAChangefeedSwap_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "items")
	before := rollbackFeeds(directory, "      gone:\n        mode: UPDATES\n        format: JSON\n        consumers:\n          reader: {}\n")
	after := rollbackFeeds(directory, "      fresh:\n        mode: KEYS_ONLY\n        format: JSON\n")
	created, createErr := hostApplySource(c, directory, "yaml", before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))

	migrations, _, down := hostGenerate(c, directory, "yaml", after)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s", applied))
	feeds, found := describedChangefeeds(c, opened, absolute)
	c.Assert(found, qt.IsTrue)
	c.Assert(feeds, qt.DeepEquals, map[string]feedShape{"fresh": {Mode: options.ChangefeedModeKeysOnly, Retention: 24 * time.Hour}})
	c.Assert(hostPlanned(c, directory, "yaml", after), qt.HasLen, 0)

	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	c.Assert(downErr, qt.IsNil, qt.Commentf("%s\n%s", rolledBack, down))
	c.Assert(down, qt.Contains, "Recovery limit:")
	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/items` DROP CHANGEFEED `fresh`;")
	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/items` ADD CHANGEFEED `gone` WITH (MODE = 'UPDATES', FORMAT = 'JSON');")
	c.Assert(down, qt.Contains, "ALTER TOPIC `"+directory+"/items/gone` ADD CONSUMER `reader`;")
	feeds, found = describedChangefeeds(c, opened, absolute)
	c.Assert(found, qt.IsTrue)
	c.Assert(feeds, qt.DeepEquals, map[string]feedShape{"gone": {Mode: options.ChangefeedModeUpdates, Retention: 24 * time.Hour, Consumers: []string{"reader"}}})
	c.Assert(hostPlanned(c, directory, "yaml", before), qt.HasLen, 0)
}
