// Package coordinationcompare compares captured YDB coordination states offline.
package coordinationcompare

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// ServiceID names the owner of coordination object comparisons.
const ServiceID = "ydb-coordination"

// Compare evaluates complete operands and coverage without database access.
// The backend validates the negotiated SDK request and result around this call.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	return objectcompare.Compare(ctx, request, family())
}

func family() objectcompare.Family {
	return objectcompare.Family{
		ServiceID: ServiceID, Kind: schema.CoordinationProperties, Capability: "coordination_nodes", Noun: "coordination node",
		DesiredModel:     schema.CoordinationDesiredModel,
		Owner:            schema.CoordinationBoundOwner,
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize,
		Reserved: func(subject *protocol.ComparisonIdentity) string {
			if subject.GetSchema().GetNormalized() == "" && subject.GetName().GetNormalized() == "ptah_locks" {
				return "The runtime lock node requires an explicit ownership and lifecycle contract"
			}
			return ""
		},
		Unsettled: schema.ValidateCoordinationAccess,
		Equal:     equal,
		Change:    schema.CaptureCoordinationChange,
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			config, err := schema.CoordinationConfiguration(current, protocol.ValueInput_OBSERVED)
			if err != nil {
				return nil, err
			}
			return schema.CaptureCoordinationIntent(owner, config)
		},
	}
}

// equal compares the effective settings: an omitted setting requests the
// server's default, so it equals the default the node holds.
func equal(current, desired *protocol.Envelope) (bool, error) {
	before, err := schema.CoordinationConfiguration(current, protocol.ValueInput_OBSERVED)
	if err != nil {
		return false, err
	}
	after, err := schema.CoordinationConfiguration(desired, protocol.ValueInput_DESIRED)
	if err != nil {
		return false, err
	}
	a, err := schema.EffectiveCoordination(before)
	if err != nil {
		return false, err
	}
	b, err := schema.EffectiveCoordination(after)
	if err != nil {
		return false, err
	}
	return a == b, nil
}
