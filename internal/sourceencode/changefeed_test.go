package sourceencode_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/encoding/protojson"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A table's changefeeds are written with every option a changefeed and its
// consumers take, and read back as written: resolved timestamps, an initial
// scan, user SIDs, schema changes, the topic's partitions and
// auto-partitioning, a consumer's read_from, codecs and availability
// period, and a second changefeed of the table, the set in name order.
// Ported from the host's TestRender_Changefeed.
func TestEncodeTable_WritesEveryChangefeedOption(t *testing.T) {
	feeds := []ydbdecl.Changefeed{
		{Name: "keys", Mode: "KEYS_ONLY", Format: "JSON"},
		{Name: "updates", Mode: "NEW_AND_OLD_IMAGES", Format: "JSON", VirtualTimestamps: true, ResolvedTimestamps: "PT10S", InitialScan: true, UserSIDs: true,
			SchemaChanges: true, TopicMinActivePartitions: 2, TopicAutoPartitioning: true, RetentionPeriod: "PT12H",
			Consumers: []ydbdecl.Consumer{{Name: "audit", Important: true, ReadFrom: "2026-01-01T00:00:00Z", SupportedCodecs: []string{"raw", "gzip"}},
				{Name: "late", AvailabilityPeriod: "PT1H"}}},
	}
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			var set native.ChangefeedSetIntent
			want := make([]changefeeds.Feed, 0, len(feeds))
			for _, feed := range feeds {
				intent, err := changefeeds.Intent(feed)
				c.Assert(err, qt.IsNil)
				set.Changefeeds = append(set.Changefeeds, intent)
				want = append(want, changefeeds.Feed{Changefeed: feed})
			}
			payload, err := protojson.Marshal(&set)
			c.Assert(err, qt.IsNil)

			result, err := sourceencode.EncodeTable(format, table(c, facet{"feeds", schema.ChangefeedProperties, "", string(payload)}))

			c.Assert(err, qt.IsNil)
			c.Assert(result.Losses, qt.HasLen, 0)
			c.Assert(decode(c, format, result.Parts).feeds, qt.DeepEquals, want)
		})
	}
}
