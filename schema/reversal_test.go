package schema_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// streamingTextChange is a change of a stopped query's text that the
// declaration permits to reset the query's state.
func streamingTextChange(c *qt.C) *protocol.NormalizedValue {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "app", Name: "copy"}
	read := proto.CloneOf(owner)
	read.Database = "/local"
	observed, err := schema.CaptureObject(read, &native.StreamingQuerySettings{Text: "INSERT INTO `app/sink` SELECT * FROM `app/source`;", Run: new(false), ResourcePool: "default"})
	c.Assert(err, qt.IsNil)
	observed.Owner = proto.CloneOf(owner)
	desired, err := schema.CaptureStreamingQueryIntent(owner, streaming.Spec{Text: "INSERT INTO `app/sink` SELECT * FROM `app/next`;", Run: new(false)}, true)
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureStreamingQueryChange("change", owner, observed, desired)
	c.Assert(err, qt.IsNil)
	return change
}

// The reverse of a text change asks for the text the query held, and keeps
// the permission to reset its state, which the reverse needs as much as the
// forward change did. What the forward change leaves is the declared text.
func TestReverseObjectChange_KeepsAStreamingQuerysStateReset(t *testing.T) {
	c := testcheck.New(t)

	reversed, err := schema.ReverseObjectChange("reverse", streamingTextChange(c).Value, schema.NativeAccess)

	c.Assert(err, qt.IsNil)
	restored, left := &native.StreamingQueryIntent{}, &native.StreamingQuerySettings{}
	c.Assert(protojson.Unmarshal(reversed.Restored.Payload, restored), qt.IsNil)
	c.Assert(protojson.Unmarshal(reversed.Left.Payload, left), qt.IsNil)
	c.Assert(restored.Text, qt.Equals, "INSERT INTO `app/sink` SELECT * FROM `app/source`;")
	c.Assert(restored.AllowStateReset, qt.IsTrue)
	c.Assert(left.Text, qt.Equals, "INSERT INTO `app/sink` SELECT * FROM `app/next`;")
	c.Assert(reversed.Kind, qt.Equals, schema.StreamingQueryProperties)
	c.Assert(reversed.Value.Value.Kind, qt.Equals, schema.StreamingQueryChange)
	c.Assert(reversed.Value.Id, qt.Equals, "reverse")
}

// A change of a kind no reversal knows is refused.
func TestReverseObjectChange_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	change := streamingTextChange(c).Value
	change.Kind = schema.TablePropertiesChange

	reversed, err := schema.ReverseObjectChange("reverse", change, schema.NativeAccess)

	c.Assert(err, qt.ErrorMatches, `YDB change .*row-table-properties-change has no reversal`)
	c.Assert(reversed.Value, qt.IsNil)
}

// observedPartitioning is a table read with a minimum partition count the
// declaration below does not state.
const observedPartitioning = `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],"partitioningSettings":{"minPartitionsCount":"3"}}`

// The reverse of a TTL a declaration adds removes it again: what the forward
// change leaves is the table as it was read, its partitioning included, with
// the declared TTL in place, and the reverse asks for the table as it was
// read.
func TestReverseTableChange_PredictsTheAlteredTable(t *testing.T) {
	c := testcheck.New(t)
	before := tableOperand(c, protocol.ValueInput_OBSERVED, observedPartitioning)
	after := tableOperand(c, protocol.ValueInput_DESIRED, ttlDesired)
	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), before, after)
	c.Assert(err, qt.IsNil)

	reversed, err := schema.ReverseTableChange("reverse", change.Value, schema.NativeAccess)

	c.Assert(err, qt.IsNil)
	left, restored := &native.DescribeTableResult{}, &native.RowTableIntent{}
	c.Assert(protojson.Unmarshal(reversed.Left.Payload, left), qt.IsNil)
	c.Assert(protojson.Unmarshal(reversed.Restored.Payload, restored), qt.IsNil)
	c.Assert(left.GetTtlSettings().GetDateTypeColumn().GetExpireAfterSeconds(), qt.Equals, uint32(3600))
	c.Assert(left.PrimaryKey, qt.DeepEquals, []string{"id"})
	c.Assert(left.GetPartitioningSettings().GetMinPartitionsCount(), qt.Equals, uint64(3))
	c.Assert(restored.TtlSettings, qt.IsNil)
	c.Assert(restored.GetPartitioningSettings().GetMinPartitionsCount(), qt.Equals, uint64(3))
	c.Assert(reversed.Kind, qt.Equals, schema.TableProperties)
	c.Assert(reversed.Value.Value.Kind, qt.Equals, schema.TablePropertiesChange)
}

// observedDefaults is a table read with the settings YDB gives every new
// table, and minimumThree a declaration that raises its minimum partition
// count.
const (
	observedDefaults = `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],` +
		`"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"}}`
	minimumThree = `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],"partitioningSettings":{"minPartitionsCount":"3"}}`
)

// The reverse of a changed partitioning asks for every setting the table held,
// those YDB gives a new table included, as the host's embedded engine
// restored them: a declaration that leaves one out keeps what the table
// holds, so a reverse without them would plan nothing. A read converted for
// an export leaves them out.
func TestReverseTableChange_RestoresTheNewTableDefaults(t *testing.T) {
	c := testcheck.New(t)
	before := tableOperand(c, protocol.ValueInput_OBSERVED, observedDefaults)
	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), before, tableOperand(c, protocol.ValueInput_DESIRED, minimumThree))
	c.Assert(err, qt.IsNil)

	reversed, err := schema.ReverseTableChange("reverse", change.Value, schema.NativeAccess)

	c.Assert(err, qt.IsNil)
	restored := &native.RowTableIntent{}
	c.Assert(protojson.Unmarshal(reversed.Restored.Payload, restored), qt.IsNil)
	c.Assert(proto.Equal(restored.PartitioningSettings, &Ydb_Table.PartitioningSettings{PartitioningBySize: Ydb.FeatureFlag_ENABLED, PartitionSizeMb: 2048,
		PartitioningByLoad: Ydb.FeatureFlag_DISABLED, MinPartitionsCount: 1}), qt.IsTrue, qt.Commentf("%v", restored.PartitioningSettings))
	c.Assert(restored.KeyBloomFilter, qt.Equals, Ydb.FeatureFlag_DISABLED)
	exported, err := schema.ConvertTable(before, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	declared := &native.RowTableIntent{}
	c.Assert(protojson.Unmarshal(exported.Payload, declared), qt.IsNil)
	c.Assert(declared.PartitioningSettings, qt.IsNil)
	c.Assert(declared.KeyBloomFilter, qt.Equals, Ydb.FeatureFlag_STATUS_UNSPECIFIED)
}

// columnFamilyChange is a change that moves a column holding a literal
// default from the default family into family cold, under a declaration that
// states no default.
func columnFamilyChange(c *qt.C) *protocol.NormalizedValue {
	c.Helper()
	held := &native.ColumnMeta{DefaultValue: &native.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{Type: primitive(Ydb.Type_UTF8),
		Value: &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: "untitled"}}}}}
	before, err := schema.CaptureColumn(columnOwner(), held)
	c.Assert(err, qt.IsNil)
	declared, err := schema.CaptureColumn(columnOwner(), &native.ColumnMeta{Family: "cold"})
	c.Assert(err, qt.IsNil)
	after, err := schema.ConvertColumn(declared, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureTablePropertiesChange("change", columnOwner(), before, after)
	c.Assert(err, qt.IsNil)
	return change
}

// The reverse of a column's move into another family moves it back: what the
// forward change leaves is the column in the declared family with the default
// the declaration does not state kept, and the reverse asks for the column as
// it was read.
func TestReverseTableChange_MovesAColumnBack(t *testing.T) {
	c := testcheck.New(t)

	reversed, err := schema.ReverseTableChange("reverse", columnFamilyChange(c).Value, schema.NativeAccess)

	c.Assert(err, qt.IsNil)
	left, restored := &native.ColumnMeta{}, &native.ColumnIntent{}
	c.Assert(protojson.Unmarshal(reversed.Left.Payload, left), qt.IsNil)
	c.Assert(protojson.Unmarshal(reversed.Restored.Payload, restored), qt.IsNil)
	c.Assert(left.Family, qt.Equals, "cold")
	c.Assert(left.GetFromLiteral().GetValue().GetTextValue(), qt.Equals, "untitled")
	c.Assert(restored.Family, qt.Equals, "")
	c.Assert(restored.GetLiteral().GetValue().GetTextValue(), qt.Equals, "untitled")
	c.Assert(reversed.Kind, qt.Equals, schema.ColumnProperties)
	c.Assert(proto.Equal(reversed.Left.Owner, columnOwner()), qt.IsTrue)
	c.Assert(reversed.Value.Value.Kind, qt.Equals, schema.TablePropertiesChange)
	c.Assert(reversed.Value.Id, qt.Equals, "reverse")
}

// A change whose owner is not a table, a column or an index is refused, and
// so is a change under an index owner whose operands are not an index's
// settings.
func TestReverseTableChange_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, owner, want string }{
		{"a view owner", "view", `.*table, column or index owner.*`},
		{"an index owner with a column's operands", "index", `invalid YDB observation JSON`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			change := columnFamilyChange(c).Value
			change.Owner.Kind = test.owner

			reversed, err := schema.ReverseTableChange("reverse", change, schema.NativeAccess)

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(reversed.Value, qt.IsNil)
		})
	}
}

// observedIndexSettings is a read index facet with payload, as the read
// writes it.
func observedIndexSettings(c *qt.C, payload string) *protocol.Envelope {
	c.Helper()
	held := &native.GlobalIndexSettings{}
	c.Assert(protojson.Unmarshal([]byte(payload), held), qt.IsNil)
	observed, err := schema.CaptureIndexSettings(indexOwner(), held)
	c.Assert(err, qt.IsNil)
	return observed
}

// The reverse of a change of a global index's settings asks for every setting
// the index held, as the host's embedded engine restores them: a declaration
// that states only the settings that differ from a new index's would keep
// each setting the forward change moved away from one. What the forward
// change leaves is what the index held with the declared settings in place of
// its own, as YDB reads it back: a size turns splitting by size on, an index
// that stops splitting by size reports no size, and replicas declared as none
// are none. No declaration removes a maximum partition count, so an index that
// held none declares none.
func TestReverseTableChange_RestoresEveryIndexSetting(t *testing.T) {
	const (
		defaults = `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"1"}`
		tuned    = `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"2"}`
		none     = `"readReplicasSettings":{"perAzReadReplicasCount":"0"}`
	)
	for _, test := range []struct {
		name, held, declared, left, restored string
	}{
		{name: "partitioning by load and a minimum", held: `{` + tuned + `}`,
			declared: `{"partitioningSettings":{"minPartitionsCount":"3","partitioningByLoad":"ENABLED"}}`,
			left:     `{"partitioningSettings":{"minPartitionsCount":"3","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","partitioningBySize":"ENABLED"}}`,
			restored: `{"partitioningSettings":{"minPartitionsCount":"2","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"},` + none + `}`},
		{name: "read replicas removed", held: `{` + defaults + `,"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`, declared: `{` + none + `}`,
			left:     `{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"}}`,
			restored: `{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"},"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`},
		{name: "a maximum the index did not hold", held: `{` + defaults + `}`, declared: `{"partitioningSettings":{"maxPartitionsCount":"9"}}`,
			left:     `{"partitioningSettings":{"maxPartitionsCount":"9","minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"}}`,
			restored: `{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"},` + none + `}`},
		{name: "a size where the index did not split by size",
			held:     `{"partitioningSettings":{"partitioningBySize":"DISABLED","partitioningByLoad":"DISABLED","minPartitionsCount":"1"}}`,
			declared: `{"partitioningSettings":{"partitionSizeMb":"512"}}`,
			left:     `{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"512","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"}}`,
			restored: `{"partitioningSettings":{"minPartitionsCount":"1","partitioningByLoad":"DISABLED","partitioningBySize":"DISABLED"},` + none + `}`},
		{name: "splitting by size turned off", held: `{` + defaults + `}`, declared: `{"partitioningSettings":{"partitioningBySize":"DISABLED"}}`,
			left:     `{"partitioningSettings":{"minPartitionsCount":"1","partitioningByLoad":"DISABLED","partitioningBySize":"DISABLED"}}`,
			restored: `{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"},` + none + `}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			change, err := schema.CaptureTablePropertiesChange("change", indexOwner(), observedIndexSettings(c, test.held), desiredIndexSettings(test.declared))
			c.Assert(err, qt.IsNil)

			reversed, err := schema.ReverseTableChange("reverse", change.Value, schema.NativeAccess)

			c.Assert(err, qt.IsNil)
			c.Assert(string(reversed.Left.Payload), qt.Equals, test.left)
			c.Assert(string(reversed.Restored.Payload), qt.Equals, test.restored)
			c.Assert(reversed.Kind, qt.Equals, schema.IndexSettingsProperties)
			c.Assert(proto.Equal(reversed.Left.Owner, indexOwner()), qt.IsTrue)
			c.Assert(reversed.Value.Value.Kind, qt.Equals, schema.TablePropertiesChange)
			c.Assert(reversed.Value.Id, qt.Equals, "reverse")
		})
	}
}

// The reverse of a change of a column family's settings asks for the
// settings the family held: what the forward change leaves is the family as
// declared, and the reverse restores its storage pool and its compression.
// Ported from the host's TestReverseColumnFamilies_RestoresThePriorFamilies.
func TestReverseTableChange_RestoresAFamilysSettings(t *testing.T) {
	c := testcheck.New(t)
	before := tableOperand(c, protocol.ValueInput_OBSERVED, `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"cold","data":{"media":"hdd"},"compression":"COMPRESSION_LZ4"}]}`)
	after := tableOperand(c, protocol.ValueInput_DESIRED, `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"cold","data":{"media":"ssd"},"compression":"COMPRESSION_NONE"}]}`)
	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), before, after)
	c.Assert(err, qt.IsNil)

	reversed, err := schema.ReverseTableChange("reverse", change.Value, schema.NativeAccess)

	c.Assert(err, qt.IsNil)
	left, restored := &native.DescribeTableResult{}, &native.RowTableIntent{}
	c.Assert(protojson.Unmarshal(reversed.Left.Payload, left), qt.IsNil)
	c.Assert(protojson.Unmarshal(reversed.Restored.Payload, restored), qt.IsNil)
	c.Assert(left.ColumnFamilies[1].GetData().GetMedia(), qt.Equals, "ssd")
	c.Assert(left.ColumnFamilies[1].GetCompression(), qt.Equals, Ydb_Table.ColumnFamily_COMPRESSION_NONE)
	c.Assert(restored.ColumnFamilies[1].GetName(), qt.Equals, "cold")
	c.Assert(restored.ColumnFamilies[1].GetData().GetMedia(), qt.Equals, "hdd")
	c.Assert(restored.ColumnFamilies[1].GetCompression(), qt.Equals, Ydb_Table.ColumnFamily_COMPRESSION_LZ4)
}

// The changefeed sets the reversal tests below change: a held changefeed f
// whose topic has four partitions and a consumer, the same changefeed
// declared with a longer retention, declared in another mode, and no
// changefeed at all.
const (
	heldFeeds      = `{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{"minActivePartitions":"4"},"consumers":[{"name":"audit"}]}}]}`
	retainedFeeds  = `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{},"retentionPeriod":"43200s","consumers":[{"name":"audit"}]}}]}`
	recreatedFeeds = `{"changefeeds":[{"name":"f","mode":"MODE_NEW_IMAGE","format":"FORMAT_JSON","topic":{"partitioningSettings":{},"consumers":[{"name":"audit"}]}}]}`
	noFeeds        = `{"changefeeds":[]}`
)

// reversedFeeds reverses the change of app.records's changefeeds from before,
// read, to after, declared.
func reversedFeeds(c *qt.C, before, after string) (schema.Reversed, error) {
	c.Helper()
	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), changefeedSet(c, protocol.ValueInput_OBSERVED, before), changefeedSet(c, protocol.ValueInput_DESIRED, after))
	c.Assert(err, qt.IsNil)
	return schema.ReverseTableChange("reverse", change.Value, schema.NativeAccess)
}

// The reverse of a changefeed's drop declares it again as the table held it,
// its consumer and its topic's partitions included. Ported from the host's
// TestRegisteredYDBReversalRestoresDefinitionsAndReportsStateLoss.
func TestReverseTableChange_RestoresADroppedChangefeed(t *testing.T) {
	c := testcheck.New(t)

	reversed, err := reversedFeeds(c, heldFeeds, noFeeds)

	c.Assert(err, qt.IsNil)
	c.Assert(string(reversed.Left.Payload), qt.Equals, `{}`)
	c.Assert(string(reversed.Restored.Payload), qt.Equals,
		`{"changefeeds":[{"format":"FORMAT_JSON","mode":"MODE_UPDATES","name":"f","topic":{"consumers":[{"name":"audit"}],"partitioningSettings":{"minActivePartitions":"4"}}}]}`)
	c.Assert(reversed.Kind, qt.Equals, schema.ChangefeedProperties)
}

// What a change of a changefeed's topic in place leaves keeps the partitions
// the topic held, which ALTER TOPIC does not change; what a changefeed added
// again leaves states none, since YDB derives them from the table. Either
// reverse declares the partitions the topic held. Ported from the host's
// TestReversalPreservesOnlyEstablishedPartitionCounts.
func TestReverseTableChange_KeepsAnEstablishedPartitionCount(t *testing.T) {
	for _, test := range []struct {
		name, after, left string
	}{
		{"a topic changed in place", retainedFeeds,
			`{"changefeeds":[{"description":{"format":"FORMAT_JSON","mode":"MODE_UPDATES","name":"f","state":"STATE_ENABLED"},` +
				`"topic":{"consumers":[{"name":"audit"}],"partitioningSettings":{"minActivePartitions":"4"},"retentionPeriod":"43200s"}}]}`},
		{"a changefeed added again", recreatedFeeds,
			`{"changefeeds":[{"description":{"format":"FORMAT_JSON","mode":"MODE_NEW_IMAGE","name":"f","state":"STATE_ENABLED"},` +
				`"topic":{"consumers":[{"name":"audit"}],"partitioningSettings":{}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			reversed, err := reversedFeeds(c, heldFeeds, test.after)

			c.Assert(err, qt.IsNil)
			c.Assert(string(reversed.Left.Payload), qt.Equals, test.left)
			restored := &native.ChangefeedSetIntent{}
			c.Assert(protojson.Unmarshal(reversed.Restored.Payload, restored), qt.IsNil)
			c.Assert(restored.Changefeeds[0].GetTopic().GetPartitioningSettings().GetMinActivePartitions(), qt.Equals, int64(4))
		})
	}
}

// A changefeed an asynchronous replication controller owns has no reverse:
// the controller creates and removes it, so the reverse of a change that
// drops or declares it is refused. Ported from the host's
// TestReversalRefusesIndependentReplicationStreamOperations.
func TestReverseTableChange_RefusesAReplicationManagedChangefeed(t *testing.T) {
	const binding = `"attributes":{"__async_replication":"{\"path\":\"/local/repl/records\",\"id\":\"1\",\"supports_topic_autopartitioning\":false}"}`
	for _, test := range []struct {
		name, before, after string
	}{
		{"a drop", `{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED",` + binding + `},"topic":{"partitioningSettings":{}}}]}`, noFeeds},
		{"a creation", noFeeds, `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON",` + binding + `,"topic":{"partitioningSettings":{}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			reversed, err := reversedFeeds(c, test.before, test.after)

			c.Assert(err, qt.ErrorMatches, `the changefeed change has no reverse: changefeed "f" is declared as a replication-managed stream, .*`)
			c.Assert(reversed.Value, qt.IsNil)
		})
	}
}

// A changefeed change that leaves an operand out cannot be reconstructed, so
// it has no reverse. Ported from the host's
// TestReversalRefusesUnreconstructibleOrMalformedChanges.
func TestReverseTableChange_RefusesAChangefeedChangeWithoutAnOperand(t *testing.T) {
	c := testcheck.New(t)
	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), changefeedSet(c, protocol.ValueInput_OBSERVED, heldFeeds), changefeedSet(c, protocol.ValueInput_DESIRED, noFeeds))
	c.Assert(err, qt.IsNil)
	forged := payload(change.Value, `{"after":null,"before":`+heldFeeds+`,"facet":"`+schema.ChangefeedProperties+`"}`)

	reversed, err := schema.ReverseTableChange("reverse", forged, schema.NativeAccess)

	c.Assert(err, qt.ErrorMatches, `table properties change requires both operands`)
	c.Assert(reversed.Value, qt.IsNil)
}

// The reverse of a change that did not permit a reset of the query's state
// cannot grant one: a text change without the permission is refused, and the
// reverse of a change of how the query runs declares no permission. Ported
// from the host's TestStreamingReversalCannotInventResetPermission and
// TestStreamingSettingReversalDoesNotClaimCheckpointLoss.
func TestReverseObjectChange_GrantsNoStateReset(t *testing.T) {
	c := testcheck.New(t)
	permitted := streamingTextChange(c).Value
	unpermitted := payload(permitted, strings.Replace(string(permitted.Payload), `"allowStateReset":true,`, "", 1))

	refused, err := schema.ReverseObjectChange("reverse", unpermitted, schema.NativeAccess)

	c.Assert(err, qt.ErrorMatches, `streaming query "app/copy": changing text resets aggregation state; declare allow_state_reset=true to permit it`)
	c.Assert(refused.Value, qt.IsNil)

	read, err := schema.Normalize(forgedValue(schema.StreamingQueryProperties, protocol.ValueInput_OBSERVED, streamingOwner(), `{"text":"SELECT 1;"}`))
	c.Assert(err, qt.IsNil)
	stopped, err := schema.CaptureStreamingQueryIntent(streamingOwner(), streaming.Spec{Text: "SELECT 1;", Run: new(false)}, false)
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureStreamingQueryChange("change", streamingOwner(), read, stopped)
	c.Assert(err, qt.IsNil)
	reversed, err := schema.ReverseObjectChange("reverse", change.Value, schema.NativeAccess)
	c.Assert(err, qt.IsNil)
	c.Assert(string(reversed.Restored.Payload), qt.Equals, `{"text":"SELECT 1;"}`)
}
