package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
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

	reversed, err := schema.ReverseTableChange("reverse", columnFamilyChange(c).Value, false)

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
// so is a change of an index's settings, whose reverse would have to state
// every setting the index held.
func TestReverseTableChange_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, owner, want string }{
		{"a view owner", "view", `.*table, column or index owner.*`},
		{"an index owner", "index", `a change of a global index's settings has no reversal yet`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			change := columnFamilyChange(c).Value
			change.Owner.Kind = test.owner

			reversed, err := schema.ReverseTableChange("reverse", change, false)

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(reversed.Value, qt.IsNil)
		})
	}
}
