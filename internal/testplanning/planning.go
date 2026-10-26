// Package testplanning constructs captured native planning exchanges for tests.
package testplanning

import (
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// Request preserves actual complete operands under the given object identity.
func Request(owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.PlanFeaturesRequest, error) {
	change, err := schema.CaptureCoordinationChange("change", owner, before, after)
	if err != nil {
		return nil, err
	}
	return &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: coordinationplan.ServiceID, DatabasePath: owner.Database, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Capabilities: map[string]bool{"coordination_nodes": true, "transactional_ddl": false}, Changes: []*protocol.PlanningChange{{Subject: Subject(owner), Value: change}}}, nil
}

// Subject keeps database, directory, and leaf identity separate.
func Subject(owner *protocol.Reference) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: owner.Kind, Database: testcomparison.Part(owner.Database), Schema: testcomparison.Part(owner.Schema), Name: testcomparison.Part(owner.Name)}
}

// RenderRequest captures the planned native operations without scheduling changes.
// Tests use it only for a single provider contribution without common steps.
func RenderRequest(plan *protocol.PlannedFeatures, capabilities map[string]bool) *protocol.RenderRequest {
	request := &protocol.RenderRequest{Engine: "ydb", TargetFeatures: backend.Description().Engines[0].Features, Capabilities: capabilities}
	for _, contribution := range plan.Contributions {
		for _, step := range contribution.Steps {
			value := proto.CloneOf(step.Operation.Value)
			request.Changes = append(request.Changes, &protocol.Change{Id: value.Id, Object: proto.CloneOf(value.Value.Owner), Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: step.Operation.Role, Value: value}}})
		}
	}
	return request
}
