package tablecreate_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A changefeed change writes, in the TEXT form, the statements the host's
// embedded engine writes into a migration file for the same change, in its
// order (its TestGenerateMigrationAST_Changefeeds_HappyPath, there on a table
// named items): the drops, a changed changefeed among them, then the
// additions with the consumers of each topic, then each topic's change in
// place, the retention first, and a consumer whose codecs are taken away
// dropped and added again.
func TestChangefeedTextForm_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	held := `{"changefeeds":[` +
		`{"description":{"name":"gone","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{},"consumers":[{"name":"reader"}]}},` +
		`{"description":{"name":"moved","mode":"MODE_KEYS_ONLY","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{},"consumers":[{"name":"a"},{"name":"b"}]}},` +
		`{"description":{"name":"kept","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},` +
		`"topic":{"partitioningSettings":{},"retentionPeriod":"21600s","consumers":[{"name":"audit","supportedCodecs":{"codecs":[1]}}]}}]}`
	declared := `{"changefeeds":[` +
		`{"name":"moved","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{},"consumers":[{"name":"a"},{"name":"b"}]}},` +
		`{"name":"kept","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{},"consumers":[{"name":"audit"}]}},` +
		`{"name":"fresh","mode":"MODE_NEW_IMAGE","format":"FORMAT_DEBEZIUM_JSON","initialScan":true,"topic":{"partitioningSettings":{}}}]}`
	request := alterRequest(c, feedCase(declared, held))
	_, plan := planned(c, request)

	units := textUnits(c, plan, request.Capabilities)

	c.Assert(units, qt.DeepEquals, []string{strings.Join([]string{
		"ALTER TABLE `app/records` DROP CHANGEFEED `gone`;",
		"ALTER TABLE `app/records` DROP CHANGEFEED `moved`;",
		"ALTER TABLE `app/records` ADD CHANGEFEED `fresh` WITH (MODE = 'NEW_IMAGE', FORMAT = 'DEBEZIUM_JSON', INITIAL_SCAN = TRUE);",
		"ALTER TABLE `app/records` ADD CHANGEFEED `moved` WITH (MODE = 'UPDATES', FORMAT = 'JSON');",
		"ALTER TOPIC `app/records/moved` ADD CONSUMER `a`;",
		"ALTER TOPIC `app/records/moved` ADD CONSUMER `b`;",
		"ALTER TOPIC `app/records/kept` SET (retention_period = Interval('P1D'));",
		"ALTER TOPIC `app/records/kept` DROP CONSUMER `audit`;",
		"ALTER TOPIC `app/records/kept` ADD CONSUMER `audit`;",
	}, "\n")})
}

// A rebuild of a table that holds changefeeds writes the statements the
// host's embedded engine writes for it (its
// TestGenerateMigrationAST_Changefeeds_RebuildCarriesThem): the changefeed is
// dropped before the old table moves and added with its consumer once the
// new table holds the name.
func TestRebuildTextFormCarriesChangefeeds_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := feedRebuildRequest(c, "", heldUpdates, newest())
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)

	units := textUnits(c, plan, request.Capabilities)

	c.Assert(units, qt.HasLen, 7)
	c.Assert(units[2:], qt.DeepEquals, []string{
		"ALTER TABLE `app/records` DROP CHANGEFEED `updates`;",
		"ALTER TABLE `app/records` RENAME TO `app/__ptah_replaced_records`;",
		"ALTER TABLE `app/__ptah_rebuild_records` RENAME TO `app/records`;",
		"ALTER TABLE `app/records` ADD CHANGEFEED `updates` WITH (MODE = 'UPDATES', FORMAT = 'JSON');\n" +
			"ALTER TOPIC `app/records/updates` ADD CONSUMER `audit` WITH (important = TRUE);",
		"DROP TABLE `app/__ptah_replaced_records`;",
	})
}
