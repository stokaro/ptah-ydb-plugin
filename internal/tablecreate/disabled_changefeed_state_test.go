package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// heldDisabledState is a read of app.records holding the disabled changefeed
// held, whose topic keeps YDB's retention.
const heldDisabledState = `{"changefeeds":[{"description":{"name":"held","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_DISABLED"},` +
	`"topic":{"partitioningSettings":{}}}]}`

// A disabled changefeed the table holds, and the declaration states disabled,
// stays where it is: adding another changefeed adds only that one, and a
// changed retention of its topic is the one ALTER TOPIC that sets it. Neither
// drops it and adds it again, which would enable it, and a topic change
// disables nothing, so the target needs no statement that disables one.
// Ported from the host's
// TestGenerateMigrationAST_Changefeeds_PreservesDisabledStateInPlace.
func TestChangefeedPlanKeepsADisabledChangefeed_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, desired string
		want          []string
	}{
		{name: "another changefeed added",
			desired: `{"changefeeds":[{"name":"held","mode":"MODE_UPDATES","format":"FORMAT_JSON","disabled":true,"topic":{"partitioningSettings":{}}},` +
				`{"name":"fresh","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`,
			want: []string{"ALTER TABLE `app/records` ADD CHANGEFEED `fresh` WITH (MODE = 'UPDATES', FORMAT = 'JSON');"}},
		{name: "its topic's retention changed",
			desired: `{"changefeeds":[{"name":"held","mode":"MODE_UPDATES","format":"FORMAT_JSON","disabled":true,` +
				`"topic":{"partitioningSettings":{},"retentionPeriod":"21600s"}}]}`,
			want: []string{"ALTER TOPIC `app/records/held` SET (retention_period = Interval('PT6H'));"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := alterRequest(c, feedCase(test.desired, heldDisabledState))

			plan, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			c.Assert(renderedText(c, plan, request.Capabilities), qt.DeepEquals, test.want)
		})
	}
}
