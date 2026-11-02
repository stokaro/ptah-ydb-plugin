package inspect

// This white-box test checks the private merge boundary. Public traversal
// returns streaming facts alone and cannot observe reservation preservation.
import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

func TestStreamingInventoryKeepsIndependentReservationConflicts(t *testing.T) {
	c := qt.New(t)
	snapshot := &protocol.Schema{Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED}}
	reserve(snapshot, &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Name: "history"}, protocol.ReservedObject_HISTORY)
	reservations := proto.CloneOf(snapshot).ReservedObjects
	var observed *protocol.ValueModel
	for _, model := range schema.ObservedModels() {
		if model.Kind == schema.StreamingQueryProperties {
			observed = model
		}
	}
	inventory := &protocol.Schema{Coverage: &protocol.ModelCoverage{Kinds: []*protocol.ModelKindCoverage{{Model: observed, Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}}}}
	replaceStreamingInventory(snapshot, inventory)
	c.Assert(snapshot.ReservedObjects, qt.CmpEquals(protocmp.Transform()), reservations)
	c.Assert(snapshot.ReservedObjects[0].ConflictKinds, qt.Contains, schema.StreamingQueryProperties)
	for _, covered := range snapshot.Coverage.Subjects {
		c.Assert(covered.Kind, qt.Not(qt.Equals), schema.StreamingQueryProperties)
	}
	c.Assert(snapshot.Coverage.Kinds[0].Knowledge.State, qt.Equals, protocol.ModelKnowledge_COMPLETE)
}
