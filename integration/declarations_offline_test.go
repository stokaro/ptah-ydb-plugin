//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/declarationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

func TestDeclarationsOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProviderListing(c, binary, digest, backend.Description().Features)
	description := client.Description()
	engine := description.Engines[0]
	c.Assert(engine.Declarations, qt.HasLen, 1)
	desired, err := schema.CaptureCoordinationIntent(&protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: "app", Name: "workers"}, schema.CoordinationIntent{SelfCheckPeriodMillis: new(uint32(2000))})
	c.Assert(err, qt.IsNil)
	normalized, err := client.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: engine.Features, Values: []*protocol.ValueInput{{Id: "node", Representation: protocol.ValueInput_DESIRED, Value: desired}}})
	c.Assert(err, qt.IsNil)
	request := &protocol.PlanDeclarationsRequest{Engine: "ydb", DeclarationId: declarationplan.ServiceID, TargetFeatures: engine.Features, Identifiers: &protocol.ComparisonIdentifiers{Rules: engine.Identifiers}, Capabilities: map[string]bool{"transactional_ddl": false, "coordination_nodes": true}, Objects: []*protocol.ComparisonRecord{{Kind: desired.Kind, Subject: testplanning.Subject(desired.Owner), Value: normalized.Values[0]}}}
	result, err := client.PlanDeclarations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Declarations, qt.HasLen, 1)
	rendering := testplanning.RenderRequest(&protocol.PlannedFeatures{Contributions: result.Contributions}, request.Capabilities)
	rendering.TargetFeatures = engine.Features
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := client.Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(rendered.Units[0].GetSql(), qt.Equals, "CREATE COORDINATION NODE `app/workers` WITH (self_check_period = Interval('PT2S'));")
	delete(request.Capabilities, "coordination_nodes")
	refused, err := client.PlanDeclarations(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(refused.Diagnostics, qt.HasLen, 1)
	c.Assert(refused.Contributions, qt.HasLen, 0)
	c.Assert(refused.Declarations, qt.HasLen, 0)
	silent := startProviderListing(c, binary, digest, nil)
	c.Assert(silent.Description().Engines[0].Declarations, qt.HasLen, 0)
	_, err = silent.PlanDeclarations(c.Context(), request)
	c.Assert(err, qt.ErrorIs, provider.ErrProtocol)
}
