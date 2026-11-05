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

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
	return specFamily[replication.ReplicationSpec]{
		serviceID: ReplicationServiceID, kind: schema.ReplicationProperties, capability: replication.CapabilityReplication,
		noun:     "async replication",
		observed: schema.ObservedReplication,
		desired:  schema.DesiredReplication,
		equal:    replication.ReplicationsEqual,
		change:   schema.CaptureReplicationChange,
		intent:   schema.CaptureReplicationIntent,
		// A replication that failed before it resolved its tables reports
		// none, and no declaration holds that.
		validate: replication.ValidateReplication,
	}.family()
}

// TransferFamily is the transfer's part of a comparison.
func TransferFamily() objectcompare.Family {
	return specFamily[replication.TransferSpec]{
		serviceID: TransferServiceID, kind: schema.TransferProperties, capability: replication.CapabilityTransfers,
		noun:     "transfer",
		observed: schema.ObservedTransfer,
		desired:  schema.DesiredTransfer,
		equal:    replication.TransfersEqual,
		change:   schema.CaptureTransferChange,
		intent:   schema.CaptureTransferIntent,
		validate: replication.ValidateTransfer,
	}.family()
}

// specFamily is what one replication object kind contributes to its
// comparison: its names, and how a spec of type S is decoded from each side,
// compared, kept and validated. Both kinds share every other part.
type specFamily[S any] struct {
	serviceID, kind, capability, noun string
	// observed decodes the read object into its spec and reported state.
	observed func(*protocol.Envelope) (S, string, error)
	// desired decodes the declared object into its spec.
	desired func(*protocol.Envelope) (S, error)
	// equal reports whether the current spec already holds the desired one.
	equal func(desired, current S) bool
	// change captures the change from the current to the desired object.
	change func(id string, owner *protocol.Reference, before, after *protocol.Envelope) (*protocol.NormalizedValue, error)
	// intent captures the desired value that keeps a spec as it is.
	intent func(*protocol.Reference, S) (*protocol.Envelope, error)
	// validate refuses a read spec no declaration can hold.
	validate func(S) error
}

// family builds the comparison family: a change needs the capability, no
// subject is reserved or unsettled, and a read object whose spec fails
// validation is reported as having no declaration that keeps it.
func (f specFamily[S]) family() objectcompare.Family {
	return objectcompare.Family{
		ServiceID: f.serviceID, Kind: f.kind, Capability: f.capability, ChangeCapability: true,
		Noun:         f.noun,
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(f.kind) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(f.kind, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize,
		Reserved:  func(*protocol.ComparisonIdentity) string { return "" },
		Unsettled: func(*protocol.Envelope, *protocol.Envelope) error { return nil },
		Equal: func(current, desired *protocol.Envelope) (bool, error) {
			before, _, err := f.observed(current)
			if err != nil {
				return false, err
			}
			after, err := f.desired(desired)
			return err == nil && f.equal(after, before), err
		},
		Change: f.change,
		Preserve: func(owner *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			spec, _, err := f.observed(current)
			if err != nil {
				return nil, err
			}
			return f.intent(owner, spec)
		},
		Unkeepable: func(current *protocol.Envelope) (string, error) {
			spec, state, err := f.observed(current)
			if err != nil {
				return "", err
			}
			if err := f.validate(spec); err != nil {
				return "The " + f.noun + " (" + stateText(state) + ") has no declaration that keeps it: " + err.Error(), nil
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
