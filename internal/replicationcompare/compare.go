// Package replicationcompare compares the declared and the read YDB async
// replications and transfers offline, as Ptah's embedded YDB engine compares
// them: each side resolved as YDB keeps it, so a declaration naming a default
// and one leaving it out are the same object, and a replication's items
// compare by the tables they replicate.
//
// Whether YDB can make a change in place depends on the state the object
// reported, so the planner decides it; the comparison reports the change.
package replicationcompare

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The comparison services this package owns.
const (
	// ReplicationServiceID names the owner of async replication comparisons.
	ReplicationServiceID = "ydb-async-replications"
	// TransferServiceID names the owner of transfer comparisons.
	TransferServiceID = "ydb-transfers"
)

// CompareReplications evaluates complete operands and coverage without
// database access. A target without the async_replication capability is
// refused only where the comparison reports a change.
func CompareReplications(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	return objectcompare.Compare(ctx, request, ReplicationFamily())
}

// CompareTransfers evaluates complete transfer operands and coverage, as
// [CompareReplications] does for replications.
func CompareTransfers(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	return objectcompare.Compare(ctx, request, TransferFamily())
}

// ReplicationFamily is the async replication's part of a comparison.
func ReplicationFamily() objectcompare.Family {
	return objectcompare.Family{
		ServiceID: ReplicationServiceID, Kind: schema.ReplicationProperties, Capability: replication.CapabilityReplication, ChangeCapability: true,
		Noun:         "async replication",
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(schema.ReplicationProperties) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.ReplicationProperties, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize,
		Reserved:  func(*protocol.ComparisonIdentity) string { return "" },
		Unsettled: func(*protocol.Envelope, *protocol.Envelope) error { return nil },
		Equal: func(current, desired *protocol.Envelope) (bool, error) {
			before, _, err := schema.ObservedReplication(current)
			if err != nil {
				return false, err
			}
			after, err := schema.DesiredReplication(desired)
			return err == nil && replication.ReplicationsEqual(after, before), err
		},
		Change: schema.CaptureReplicationChange,
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			spec, _, err := schema.ObservedReplication(current)
			if err != nil {
				return nil, err
			}
			return schema.CaptureReplicationIntent(owner, spec)
		},
		Unkeepable: func(current *protocol.Envelope) (string, error) {
			spec, state, err := schema.ObservedReplication(current)
			if err != nil {
				return "", err
			}
			if err := replication.ValidateReplication(spec); err != nil {
				// A replication that failed before it resolved its tables
				// reports none, and no declaration holds that.
				return "The async replication (" + stateText(state) + ") has no declaration that keeps it: " + err.Error(), nil
			}
			return "", nil
		},
	}
}

// TransferFamily is the transfer's part of a comparison.
func TransferFamily() objectcompare.Family {
	return objectcompare.Family{
		ServiceID: TransferServiceID, Kind: schema.TransferProperties, Capability: replication.CapabilityTransfers, ChangeCapability: true,
		Noun:         "transfer",
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(schema.TransferProperties) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.TransferProperties, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize,
		Reserved:  func(*protocol.ComparisonIdentity) string { return "" },
		Unsettled: func(*protocol.Envelope, *protocol.Envelope) error { return nil },
		Equal: func(current, desired *protocol.Envelope) (bool, error) {
			before, _, err := schema.ObservedTransfer(current)
			if err != nil {
				return false, err
			}
			after, err := schema.DesiredTransfer(desired)
			return err == nil && replication.TransfersEqual(after, before), err
		},
		Change: schema.CaptureTransferChange,
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			spec, _, err := schema.ObservedTransfer(current)
			if err != nil {
				return nil, err
			}
			return schema.CaptureTransferIntent(owner, spec)
		},
		Unkeepable: func(current *protocol.Envelope) (string, error) {
			spec, state, err := schema.ObservedTransfer(current)
			if err != nil {
				return "", err
			}
			if err := replication.ValidateTransfer(spec); err != nil {
				return "The transfer (" + stateText(state) + ") has no declaration that keeps it: " + err.Error(), nil
			}
			return "", nil
		},
	}
}

func stateText(state string) string {
	if state == "" {
		return "state not reported"
	}
	return state
}
