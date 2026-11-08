package reversal_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The reverse of a change of a table's changefeeds says what it cannot undo
// from what the change does to each changefeed: a retention the change made
// shorter has expired messages the rollback cannot bring back, as the host's
// embedded engine said, a longer one loses nothing, a changefeed the change
// added is dropped with its unread messages, and one the change recreated in
// another mode starts a new stream when the rollback adds it again. The reverse of a retention change in place said a stream was
// added again (G234).
func TestReverse_ChangefeedLimitsFollowTheChange(t *testing.T) {
	for _, test := range []struct {
		name, declared string
		want           []string
	}{
		{name: "a shorter retention", declared: `{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{},"retentionPeriod":"43200s"}}`,
			want: []string{"Restoring retention cannot recover messages that have already expired."}},
		{name: "a longer retention", declared: `{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{},"retentionPeriod":"172800s"}}`},
		{name: "a new changefeed", declared: `{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{},"retentionPeriod":"86400s"}},` +
			`{"name":"g","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}`,
			want: []string{"a changefeed the forward change added to table app/records is dropped by the rollback with its topic; its unread messages and consumer positions are lost"}},
		{name: "another mode", declared: `{"name":"f","mode":"MODE_KEYS_ONLY","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}`,
			want: []string{"a changefeed of table app/records the rollback adds again starts a new stream; the messages and the consumers' positions of the dropped one are lost"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			owner := &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "app", Kind: "table", Name: "records"}
			held := valueOf(c, schema.ChangefeedProperties, protocol.ValueInput_OBSERVED, owner,
				`{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{},"retentionPeriod":"86400s"}}]}`)
			declared := valueOf(c, schema.ChangefeedProperties, protocol.ValueInput_DESIRED, owner, `{"changefeeds":[`+test.declared+`]}`)
			change, err := schema.CaptureTablePropertiesChange("change", owner, held, declared)
			c.Assert(err, qt.IsNil)

			reverse := reversed(c, change)

			c.Assert(reverse.Strategy, qt.Equals, "restore the changefeeds the table held")
			c.Assert(reverse.Limitations, qt.DeepEquals, test.want)
		})
	}
}
