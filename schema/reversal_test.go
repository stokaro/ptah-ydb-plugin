package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
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
