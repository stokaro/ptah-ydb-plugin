package backend_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func transferContextState(c *qt.C, representation protocol.ValueInput_Representation, value *protocol.Envelope) *protocol.ComparisonState {
	c.Helper()
	var model *protocol.ValueModel
	for _, candidate := range schema.Models() {
		if candidate.Kind == schema.TransferProperties && candidate.Representation == representation {
			model = candidate
		}
	}
	state := testcomparison.State(model)
	if value != nil {
		normalized, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: backend.Description().Features,
			Values: []*protocol.ValueInput{{Id: "ingest", Representation: representation, Value: value}}})
		c.Assert(err, qt.IsNil)
		state.Records = []*protocol.ComparisonRecord{{Kind: value.Kind, Subject: testplanning.Subject(value.Owner), Value: normalized.Values[0]}}
	}
	return state
}

func keptTransferRebuildRequest(c *qt.C, spec replication.TransferSpec, tableName string) *protocol.PlanFeaturesRequest {
	c.Helper()
	declared := spec
	declared.Source = strings.TrimPrefix(declared.Source, "/local/")
	table := &protocol.PlanningTable{Subject: &protocol.ComparisonIdentity{Kind: "table", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
		Schema: testcomparison.Part("app"), Parent: &protocol.ComparisonPart{}, Name: testcomparison.Part(tableName)},
		Action: protocol.PlanningTable_REBUILD, Desired: &protocol.DeclaredTable{Name: tableName}, Current: &protocol.ObservedTable{Name: tableName}}
	return &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: replicationplan.TransferServiceID, DatabasePath: "/local",
		ContextKinds: []string{schema.TransferProperties}, TargetFeatures: backend.Description().Features, Capabilities: replicationCapabilities(),
		Identifiers:    &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		DesiredObjects: transferContextState(c, protocol.ValueInput_DESIRED, declaredTransfer(c, &declared)),
		CurrentObjects: transferContextState(c, protocol.ValueInput_OBSERVED, readTransfer(c, &spec, replication.StateRunning)),
		Tables:         []*protocol.PlanningTable{provider.TableOutline(table)}}
}

// A transfer held through the plan prevents the rebuild from replacing its
// target table or the table that owns its source changefeed. YDB may report
// the source as an absolute path within the database.
func TestKeptTransferRefusesTableRebuild_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		spec replication.TransferSpec
	}{
		{name: "target", spec: replication.TransferSpec{Source: "events", Target: "app/items", Lambda: "($m) -> { return [$m]; }"}},
		{name: "source changefeed", spec: replication.TransferSpec{Source: "app/items/feed", Target: "log", Lambda: "($m) -> { return [$m]; }"}},
		{name: "absolute source changefeed", spec: replication.TransferSpec{Source: "/local/app/items/feed", Target: "log", Lambda: "($m) -> { return [$m]; }"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := keptTransferRebuildRequest(c, test.spec, "items")
			result, err := backend.New().PlanFeatures(t.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(result.Objects, qt.HasLen, 0)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Diagnostics[0].Message, qt.Equals, `rebuilding table "app/items": transfer ingest writes the table or reads one of its changefeeds, `+
				`and a rebuild swaps the table from under it; drop the transfer, rebuild, and create it again`)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
		})
	}
}

// A rebuild of another table leaves the transfer in place and receives the
// context receipt that acknowledges it.
func TestKeptTransferAllowsAnotherTableRebuild_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	spec := replication.TransferSpec{Source: "app/items/feed", Target: "log", Lambda: "($m) -> { return [$m]; }"}
	request := keptTransferRebuildRequest(c, spec, "other")
	result, err := backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Objects, qt.HasLen, 2)
	c.Assert(result.Objects[0].Steps, qt.HasLen, 0)
	c.Assert(result.Objects[1].Steps, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
}

// Dropping a transfer before a rebuild releases its target table. The
// transfer's drop remains in the shared graph for the host to order first.
func TestDroppedTransferAllowsTableRebuild_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	spec := replication.TransferSpec{Source: "events", Target: "app/items", Lambda: "($m) -> { return [$m]; }"}
	compared, err := backend.New().CompareModels(t.Context(), objectRequest(c, schema.TransferProperties, replicationcompare.TransferServiceID, "ingest", nil,
		readTransfer(c, &spec, replication.StateRunning)))
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Changes, qt.HasLen, 1)
	request := keptTransferRebuildRequest(c, spec, "items")
	request.DesiredObjects = transferContextState(c, protocol.ValueInput_DESIRED, nil)
	request.Changes = []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value}}
	result, err := backend.New().PlanFeatures(t.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 1)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, result), qt.IsNil)
}
