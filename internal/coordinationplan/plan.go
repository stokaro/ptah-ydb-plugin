// Package coordinationplan lowers captured coordination changes into a shared graph.
package coordinationplan

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// ServiceID names the native coordination planner.
const ServiceID = "ydb-coordination"

// PathKind identifies the physical namespace shared by native YDB objects.
const PathKind = objectplan.PathKind

// Plan contributes operations, footprints, dependencies, and a receipt per input.
// The SDK boundary validates transport ownership; this service validates meaning.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	return objectplan.Plan(ctx, request, objectplan.Family{
		ServiceID: ServiceID, Kind: schema.CoordinationProperties, ChangeKind: schema.CoordinationChange,
		Capability: "coordination_nodes", Noun: "coordination node", StepPrefix: "coordination",
		Strategy:        "Apply the captured configuration through the native coordination service",
		Owner:           schema.CoordinationBoundOwner,
		NormalizeChange: schema.NormalizeCoordinationChange,
		Operation:       schema.CoordinationOperationFromChange,
		Present: func(change *protocol.Envelope) (bool, bool, error) {
			before, after, err := schema.CoordinationOperands(change)
			return before != nil, after != nil, err
		},
	})
}
