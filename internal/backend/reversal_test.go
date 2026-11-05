package backend_test

import (
	"maps"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/reversal"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// reverseOf compares request, reverses the one change the comparison returns
// and checks the reply as the host's SDK client does.
func reverseOf(c *qt.C, request *protocol.CompareModelsRequest) (*protocol.ComparedChange, *protocol.ReverseChangesRequest, *protocol.ReversedChange) {
	c.Helper()
	compared, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Changes, qt.HasLen, 1)
	change := compared.Changes[0]
	reverse := &protocol.ReverseChangesRequest{Engine: "ydb", ReversalId: reversal.ServiceID, TargetFeatures: backend.Description().Engines[0].Features,
		Identifiers: request.Identifiers, Capabilities: request.Capabilities, Changes: []*protocol.PlanningChange{{Subject: change.Subject, Value: change.Value}}}
	before := proto.CloneOf(reverse)
	reversed, err := backend.New().ReverseChanges(c.Context(), reverse)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateReversedChanges(backend.Description(), reverse, reversed), qt.IsNil)
	c.Assert(proto.Equal(before, reverse), qt.IsTrue)
	c.Assert(reversed.Changes, qt.HasLen, 1)
	return change, reverse, reversed.Changes[0]
}

// planned plans one reverse change with the planner of its kind, on a
// target that runs schema statements outside a transaction.
func planned(c *qt.C, reverse *protocol.ReverseChangesRequest, planning string, reversed *protocol.ReversedChange) *protocol.PlannedFeatures {
	c.Helper()
	capabilities := maps.Clone(reverse.Capabilities)
	capabilities["transactional_ddl"] = false
	plan, err := backend.New().PlanFeatures(c.Context(), &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: planning, TargetFeatures: reverse.TargetFeatures,
		Identifiers: reverse.Identifiers, Capabilities: capabilities, Changes: []*protocol.PlanningChange{{Subject: reversed.Subject, Value: reversed.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	return plan
}

// A coordination node the forward change creates is dropped, one it drops is
// created again with the settings it held, and one it alters gets them back
// in place. The forward state is the node the change leaves, and the reverse
// change is one the coordination planner plans.
func TestReverseChanges_UndoesACoordinationNodeChange(t *testing.T) {
	strict := Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT
	held := &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{ReadConsistencyMode: strict}, Self: &Ydb_Scheme.Entry{Owner: "original-owner"}}
	for _, test := range []struct {
		name        string
		desired     *schema.CoordinationIntent
		observed    *Ydb_Coordination.DescribeNodeResult
		strategy    string
		limitations []string
		leaves      bool
		before      bool
		after       bool
		readMode    string
	}{
		{name: "a created node", desired: &schema.CoordinationIntent{ReadConsistencyMode: new("strict")}, strategy: "drop the created coordination node",
			limitations: []string{"Dropping the new node discards its semaphores and rate limiter resources."}, leaves: true, before: true},
		{name: "a dropped node", observed: held, strategy: "recreate the dropped coordination node",
			limitations: []string{"Recreating the configuration cannot recover semaphores or rate limiter resources lost when the node was dropped."}, after: true, readMode: "strict"},
		{name: "an altered node", desired: &schema.CoordinationIntent{ReadConsistencyMode: new("relaxed")}, observed: held, strategy: "restore coordination settings in place",
			leaves: true, before: true, after: true, readMode: "strict"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := testcomparison.Request(test.desired, test.observed)
			c.Assert(err, qt.IsNil)

			change, reverse, reversed := reverseOf(c, request)

			c.Assert(reversed.Strategy, qt.Equals, test.strategy)
			c.Assert(reversed.Limitations, qt.DeepEquals, test.limitations)
			c.Assert(provider.CompareIdentities(reversed.Subject, change.Subject), qt.Equals, 0)
			c.Assert(reversed.ForwardState, qt.HasLen, 1)
			c.Assert(reversed.ForwardState[0].Placement, qt.Equals, protocol.ForwardState_OBJECT)
			c.Assert(reversed.ForwardState[0].Kind, qt.Equals, schema.CoordinationProperties)
			c.Assert(reversed.ForwardState[0].Value != nil, qt.Equals, test.leaves)
			before, after, err := schema.CoordinationOperands(reversed.Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(before != nil, qt.Equals, test.before)
			c.Assert(after != nil, qt.Equals, test.after)
			c.Assert(restoredReadMode(c, after), qt.Equals, test.readMode)
			plan := planned(c, reverse, coordinationplan.ServiceID, reversed)
			c.Assert(plan.Changes, qt.HasLen, 1)
		})
	}
}

// restoredReadMode is the read consistency mode a restored declaration asks
// for, "" where the reverse restores none.
func restoredReadMode(c *qt.C, after *protocol.Envelope) string {
	c.Helper()
	if after == nil {
		return ""
	}
	restored, err := schema.CoordinationConfiguration(after, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	return *restored.ReadConsistencyMode
}

// A created secret is dropped. A rotated one has nothing to run: the value it
// replaced was never read, and the reverse says so.
func TestReverseChanges_UndoesASecretChange(t *testing.T) {
	for _, test := range []struct {
		name        string
		read        secretRead
		rotate      bool
		strategy    string
		limitations []string
		value       bool
	}{
		{name: "a created secret", strategy: "drop the created secret", value: true},
		{name: "a rotated secret", read: secretPresent, rotate: true, strategy: "keep the secret and the value it holds",
			limitations: []string{"the value secret app/pg.pw held before the rotation was never read; the rollback keeps the rotated value"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := secretRequest(c, test.read)
			request.Requests = map[bool][]*protocol.RequestedChange{true: {{Action: "rotate", Written: "app/pg.pw"}}}[test.rotate]

			_, _, reversed := reverseOf(c, request)

			c.Assert(reversed.Strategy, qt.Equals, test.strategy)
			c.Assert(reversed.Limitations, qt.DeepEquals, test.limitations)
			c.Assert(reversed.Value != nil, qt.Equals, test.value)
			c.Assert(reversed.ForwardState, qt.HasLen, 1)
			c.Assert(reversed.ForwardState[0].Kind, qt.Equals, schema.SecretProperties)
			c.Assert(reversed.ForwardState[0].Value, qt.IsNotNil)
		})
	}
}

// The reverse of a created secret plans the secret's drop.
func TestReverseChanges_PlansTheDropOfACreatedSecret(t *testing.T) {
	c := testcheck.New(t)
	_, reverse, reversed := reverseOf(c, secretRequest(c, secretAbsent))

	before, after, err := schema.SecretOperands(reversed.Value.Value)

	c.Assert(err, qt.IsNil)
	c.Assert(before, qt.IsNotNil)
	c.Assert(after, qt.IsNil)
	plan := planned(c, reverse, secretplan.ServiceID, reversed)
	c.Assert(plan.Changes, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps[0].Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_DROP)
}

// A request the SDK refuses never reaches the reversal.
func TestReverseChanges_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
	c.Assert(err, qt.IsNil)
	compared, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	reverse := &protocol.ReverseChangesRequest{Engine: "ydb", ReversalId: "foreign", TargetFeatures: backend.Description().Engines[0].Features,
		Identifiers: request.Identifiers, Changes: []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value}}}

	reversed, err := backend.New().ReverseChanges(c.Context(), reverse)

	c.Assert(err, qt.ErrorMatches, `.*the reversal service is unavailable`)
	c.Assert(reversed, qt.IsNil)
}

// One request reverses changes of several kinds, and each prediction of the
// batch is a value of its own.
func TestReverseChanges_ReversesABatch(t *testing.T) {
	c := testcheck.New(t)
	node, err := testcomparison.Request(&schema.CoordinationIntent{}, nil)
	c.Assert(err, qt.IsNil)
	var changes []*protocol.PlanningChange
	for _, request := range []*protocol.CompareModelsRequest{node, secretRequest(c, secretAbsent)} {
		compared, err := backend.New().CompareModels(c.Context(), request)
		c.Assert(err, qt.IsNil)
		c.Assert(compared.Changes, qt.HasLen, 1)
		changes = append(changes, &protocol.PlanningChange{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value})
	}
	changes[1].Value.Id = "second"
	reverse := &protocol.ReverseChangesRequest{Engine: "ydb", ReversalId: reversal.ServiceID, TargetFeatures: backend.Description().Engines[0].Features,
		Identifiers: node.Identifiers, Changes: changes}

	reversed, err := backend.New().ReverseChanges(c.Context(), reverse)

	c.Assert(err, qt.IsNil)
	c.Assert(reversed.Changes, qt.HasLen, 2)
	c.Assert(reversed.Changes[0].Strategy, qt.Equals, "drop the created coordination node")
	c.Assert(reversed.Changes[1].Strategy, qt.Equals, "drop the created secret")
	c.Assert(reversed.Changes[0].ForwardState[0].Value.Id, qt.Not(qt.Equals), reversed.Changes[1].ForwardState[0].Value.Id)
}

// indexSettingsChange is the change of index by_label of app.records, which
// holds the settings YDB gives a new index, into the settings declared states.
func indexSettingsChange(c *qt.C, declared string) *protocol.PlanningChange {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "app", Kind: "index", Path: []string{"records"}, Name: "by_label"}
	held, err := schema.CaptureIndexSettings(owner, &native.GlobalIndexSettings{PartitioningSettings: schema.NewIndexSettings().GetPartitioningSettings()})
	c.Assert(err, qt.IsNil)
	model := schema.NativeDesiredModel(schema.IndexSettingsProperties)
	desired := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256,
		Owner: proto.CloneOf(owner), Payload: []byte(declared)}
	change, err := schema.CaptureTablePropertiesChange("change", owner, held, desired)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanningChange{Value: change, Subject: &protocol.ComparisonIdentity{Kind: "index", Database: testcomparison.Part("/local"), Catalog: testcomparison.Part(""),
		Schema: testcomparison.Part("app"), Parent: testcomparison.Part("records"), Name: testcomparison.Part("by_label")}}
}

// A change of a global index's settings reverses to every setting the index
// held, in the host's embedded engine's words, and a maximum partition count
// the forward change gives an index that held none is what the reverse cannot
// undo, since YDB removes none. The forward state is the index's own facet as
// the forward change leaves it.
func TestReverseChanges_RestoresAnIndexsSettings(t *testing.T) {
	for _, test := range []struct {
		name, declared string
		limitations    []string
	}{
		{name: "a minimum partition count", declared: `{"partitioningSettings":{"minPartitionsCount":"3"}}`},
		{name: "a maximum partition count", declared: `{"partitioningSettings":{"maxPartitionsCount":"9"}}`,
			limitations: []string{"YDB cannot remove a maximum partition count, so the rollback keeps AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 9, which the forward change set."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			reverse := &protocol.ReverseChangesRequest{Engine: "ydb", ReversalId: reversal.ServiceID, TargetFeatures: backend.Description().Engines[0].Features,
				Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
				Changes:     []*protocol.PlanningChange{indexSettingsChange(c, test.declared)}}

			reversed, err := backend.New().ReverseChanges(c.Context(), reverse)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateReversedChanges(backend.Description(), reverse, reversed), qt.IsNil)
			c.Assert(reversed.Changes, qt.HasLen, 1)
			c.Assert(reversed.Changes[0].Strategy, qt.Equals, "restore every setting the index held in place")
			c.Assert(reversed.Changes[0].Limitations, qt.DeepEquals, test.limitations)
			c.Assert(reversed.Changes[0].ForwardState, qt.HasLen, 1)
			c.Assert(reversed.Changes[0].ForwardState[0].Placement, qt.Equals, protocol.ForwardState_FACET)
			c.Assert(reversed.Changes[0].ForwardState[0].Kind, qt.Equals, schema.IndexSettingsProperties)
			c.Assert(reversed.Changes[0].ForwardState[0].Value.Value.Owner.Name, qt.Equals, "by_label")
		})
	}
}
