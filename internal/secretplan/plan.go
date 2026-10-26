// Package secretplan places value-free secret operations in the shared graph.
package secretplan

import (
	"context"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// ServiceID names the secret planning owner.
const ServiceID = "ydb-secrets"

// Plan preserves physical path and access ordering for complete secret changes.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	result, err := objectplan.Plan(ctx, request, objectplan.Family{
		ServiceID: ServiceID, Kind: schema.SecretProperties, ChangeKind: schema.SecretChange,
		Capability: secrets.Capability, Noun: "secret", StepPrefix: "secret",
		Strategy: "Create, explicitly rotate, or drop the secret using approved inputs",
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.SecretProperties, subject)
		},
		NormalizeChange: schema.NormalizeSecretChange, Operation: schema.SecretOperationFromChange,
		Present: func(value *protocol.Envelope) (bool, bool, error) {
			before, after, err := schema.SecretOperands(value)
			return before != nil, after != nil, err
		},
	})
	if err != nil || len(result.GetContributions()) == 0 {
		return result, err
	}
	if !slices.Contains(request.TargetFeatures, provider.PlanningLifecycleFeature) {
		return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE,
			Kind: schema.SecretChange, Message: "Secret dependency ordering requires planning.lifecycle", Input: &protocol.PlanningDiagnostic_Change{Change: 0}}}}, nil
	}
	for _, contribution := range result.Contributions {
		for _, step := range contribution.Steps {
			step.Operation.Phase = protocol.PlanningOperation_LIFECYCLE
		}
	}
	return result, nil
}
