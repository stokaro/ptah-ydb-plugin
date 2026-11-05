package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
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

	reversed, err := schema.ReverseObjectChange("reverse", streamingTextChange(c).Value, false)

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

	reversed, err := schema.ReverseObjectChange("reverse", change, false)

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

	reversed, err := schema.ReverseTableChange("reverse", change.Value, false)

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

// A column's change of family has no reversal: the host projects a reverse's
// state onto the table and its indexes, not onto a column.
func TestReverseTableChange_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	change := streamingTextChange(c).Value
	change.Kind = schema.TablePropertiesChange
	change.Owner.Kind = "column"

	reversed, err := schema.ReverseTableChange("reverse", change, false)

	c.Assert(err, qt.ErrorMatches, `a column's change of family has no reversal`)
	c.Assert(reversed.Value, qt.IsNil)
}
