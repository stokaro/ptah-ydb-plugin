package changefeeds_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// TestIntent_CanonicalConsumerOrderRetainsDeclarations holds what the order of
// a changefeed's consumers and codecs changes, and what a retention of YDB's
// 24 hours stated outright changes.
//
// The desired model keeps the consumers and their codecs in the order the
// declaration lists them, because the statements add them in that order, and
// the comparison reads the other order as the same changefeed. A retention of
// P1D stays in the model, so the value tells it apart from a retention left
// out, as the host's codec fingerprint did. It reads back as left out, and
// compares and plans as one: YDB keeps 24 hours in both cases and reports
// both as 86400s, and an intent converted from a read states that 86400s.
//
// Ported from the host's TestCodecs_CanonicalConsumerOrderRetainsDeclarations.
func TestIntent_CanonicalConsumerOrderRetainsDeclarations(t *testing.T) {
	c := testcheck.New(t)
	declared := ydbdecl.Changefeed{Name: "updates", Mode: "UPDATES", Format: "JSON", Consumers: []ydbdecl.Consumer{
		{Name: "worker-b", SupportedCodecs: []string{"zstd", "raw"}}, {Name: "worker-a"}}}
	reordered := ydbdecl.Changefeed{Name: "updates", Mode: "UPDATES", Format: "JSON", Consumers: []ydbdecl.Consumer{
		{Name: "worker-a"}, {Name: "worker-b", SupportedCodecs: []string{"raw", "zstd"}}}}
	explicitDefault := ydbdecl.Changefeed{Name: "updates", Mode: "UPDATES", Format: "JSON", RetentionPeriod: "P1D", Consumers: []ydbdecl.Consumer{
		{Name: "worker-b", SupportedCodecs: []string{"zstd", "raw"}}, {Name: "worker-a"}}}

	intent, err := changefeeds.Intent(declared)
	c.Assert(err, qt.IsNil)
	var names []string
	for _, consumer := range intent.GetTopic().GetConsumers() {
		names = append(names, consumer.GetName())
	}
	c.Assert(names, qt.DeepEquals, []string{"worker-b", "worker-a"})
	c.Assert(intent.GetTopic().GetConsumers()[0].GetSupportedCodecs().GetCodecs(), qt.DeepEquals,
		[]int32{int32(Ydb_Topic.Codec_CODEC_ZSTD), int32(Ydb_Topic.Codec_CODEC_RAW)})
	c.Assert(intent.GetTopic().GetRetentionPeriod(), qt.IsNil)
	read, err := changefeeds.FromIntent(intent)
	c.Assert(err, qt.IsNil)
	c.Assert(read, qt.DeepEquals, changefeeds.Feed{Changefeed: declared})

	reorderedIntent, err := changefeeds.Intent(reordered)
	c.Assert(err, qt.IsNil)
	reorderedRead, err := changefeeds.FromIntent(reorderedIntent)
	c.Assert(err, qt.IsNil)
	c.Assert(changefeeds.Equal(reorderedRead, read), qt.IsTrue)

	explicitIntent, err := changefeeds.Intent(explicitDefault)
	c.Assert(err, qt.IsNil)
	c.Assert(explicitIntent.GetTopic().GetRetentionPeriod().GetSeconds(), qt.Equals, int64(changefeeds.DefaultRetentionSeconds))
	explicitRead, err := changefeeds.FromIntent(explicitIntent)
	c.Assert(err, qt.IsNil)
	c.Assert(explicitRead, qt.DeepEquals, read)
	c.Assert(changefeeds.Equal(explicitRead, read), qt.IsTrue)
	c.Assert(changefeeds.AddStatements("app/records", explicitRead.Changefeed), qt.DeepEquals, changefeeds.AddStatements("app/records", read.Changefeed))
}
