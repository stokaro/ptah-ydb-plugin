package backend

// White-box testing required: a session's capabilities are assembled while it
// opens against a live YDB server, and no exported call returns them without
// one. The live tests under integration/ cover an opened session.

import (
	"context"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A session reports index_rename as EnableMoveIndex decides it to a host
// that lists planning.index-renames, and false to any other host, which has
// no step to send a renamed index with.
func TestIndexRenamesFollowTheHost_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		host  []string
		moves bool
		want  bool
	}{
		{"a host that renames, the flag on", []string{provider.IndexRenamesFeature}, true, true},
		{"a host that renames, the flag off", []string{provider.IndexRenamesFeature}, false, false},
		{"a host that does not rename", []string{provider.ObservedIndexesFeature}, true, false},
		{"a host that lists nothing", nil, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			held := map[string]bool{"index_rename": test.moves, "secrets": true}
			got := indexRenames(provider.ContextWithHostFeatures(context.Background(), test.host), held)
			c.Assert(got, qt.DeepEquals, map[string]bool{"index_rename": test.want, "secrets": true})
			c.Assert(held["index_rename"], qt.Equals, test.moves)
		})
	}
}

// The host changes an index change's subject to the old name when reversing a
// rename, but leaves the value bound to the forward name. Only the matching
// common rename and its footprint authorize rebinding that value for planning.
func TestRenamedIndexChangeBindingRequiresTheRenameStep(t *testing.T) {
	c := testcheck.New(t)
	part := func(name string) *protocol.ComparisonPart {
		return &protocol.ComparisonPart{Source: name, Normalized: name}
	}
	table := &protocol.ComparisonIdentity{Kind: "table", Schema: part("app"), Name: part("items")}
	old := &protocol.ComparisonIdentity{Kind: "index", Schema: part("app"), Parent: part("items"), Name: part("by_n")}
	newIndex := proto.CloneOf(old)
	newIndex.Name = part("by_number")
	owner := &protocol.Reference{Engine: "ydb", Kind: "index", Schema: "app", Path: []string{"items"}, Name: "by_number"}
	models := schema.ObservedModels()
	observedAt := slices.IndexFunc(models, func(model *protocol.ValueModel) bool { return model.Kind == schema.IndexSettingsProperties })
	c.Assert(observedAt, qt.Not(qt.Equals), -1)
	observedModel := models[observedAt]
	statedModel := schema.NativeDesiredModel(schema.IndexSettingsProperties)
	before, err := schema.Normalize(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.IndexSettingsProperties, Owner: owner,
		SchemaVersion: observedModel.SchemaVersion, Encoding: observedModel.Encoding, DefinitionSha256: observedModel.DefinitionSha256,
		Payload: []byte(`{"partitioningSettings":{"minPartitionsCount":"4"}}`)})
	c.Assert(err, qt.IsNil)
	after, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.IndexSettingsProperties, Owner: owner,
		SchemaVersion: statedModel.SchemaVersion, Encoding: statedModel.Encoding, DefinitionSha256: statedModel.DefinitionSha256,
		Payload: []byte(`{"partitioningSettings":{"minPartitionsCount":"3"}}`)})
	c.Assert(err, qt.IsNil)
	value, err := schema.CaptureTablePropertiesChange("change", owner, before, after)
	c.Assert(err, qt.IsNil)
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, TargetFeatures: []string{provider.IndexRenamesFeature},
		Changes: []*protocol.PlanningChange{{Subject: old, Value: value}},
		CommonSteps: []*protocol.CommonPlanningStep{{Parent: table, RenamedIndex: &protocol.CommonIndexRename{From: "by_number", To: "by_n"},
			Metadata: &protocol.PlanningMetadata{Effects: []*protocol.PlanningEffect{
				{Subject: table, Action: protocol.PlanningEffect_READ},
				{Subject: newIndex, Action: protocol.PlanningEffect_DROP},
				{Subject: old, Action: protocol.PlanningEffect_CREATE},
			}}}}}
	original := proto.CloneOf(request)

	rebound := rebindRenamedIndexChanges(request)

	c.Assert(rebound.Changes[0].Value.Value.Owner.Name, qt.Equals, "by_n")
	c.Assert(proto.Equal(request, original), qt.IsTrue)
	withoutStep := proto.CloneOf(request)
	withoutStep.CommonSteps = nil
	c.Assert(rebindRenamedIndexChanges(withoutStep), qt.Equals, withoutStep)
	wrongFootprint := proto.CloneOf(request)
	wrongFootprint.CommonSteps[0].Metadata.Effects[1].Subject.Name = part("another")
	c.Assert(rebindRenamedIndexChanges(wrongFootprint), qt.Equals, wrongFootprint)
	withoutFeature := proto.CloneOf(request)
	withoutFeature.TargetFeatures = nil
	c.Assert(rebindRenamedIndexChanges(withoutFeature), qt.Equals, withoutFeature)
	otherService := proto.CloneOf(request)
	otherService.PlanningId = "other"
	c.Assert(rebindRenamedIndexChanges(otherService), qt.Equals, otherService)
	forged := proto.CloneOf(request)
	forged.Changes[0].Value.CanonicalProjection = []byte("forged")
	c.Assert(rebindRenamedIndexChanges(forged), qt.Equals, forged)
}
