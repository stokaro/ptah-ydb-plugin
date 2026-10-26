// Package workloadcompare compares the declared and the read resource pools
// and classifiers of a database offline, as Ptah's embedded YDB engine
// compares them: a pool setting by setting, an unset one apart from zero, and
// a classifier as a whole. Both belong to the database, so a source that
// leaves one out keeps it.
package workloadcompare

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectcompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The comparison services, one per model kind.
const (
	PoolServiceID       = "ydb-resource-pools"
	ClassifierServiceID = "ydb-resource-pool-classifiers"
)

// ComparePools evaluates the resource pools of a request without database
// access.
func ComparePools(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	return objectcompare.Compare(ctx, request, PoolFamily())
}

// CompareClassifiers evaluates the classifiers of a request without database
// access.
func CompareClassifiers(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	return objectcompare.Compare(ctx, request, ClassifierFamily())
}

// PoolFamily is the resource pool's part of a comparison.
func PoolFamily() objectcompare.Family {
	family := base(PoolServiceID, schema.ResourcePoolProperties, "resource pool")
	family.Unsettled = unsettledPool
	family.Equal = func(current, desired *protocol.Envelope) (bool, error) {
		before, err := schema.WorkloadPool(current)
		if err != nil {
			return false, err
		}
		after, err := schema.WorkloadPool(desired)
		if err != nil {
			return false, err
		}
		return workload.PoolsEqual(after, before), nil
	}
	family.Change = schema.CaptureResourcePoolChange
	return family
}

// ClassifierFamily is the classifier's part of a comparison.
func ClassifierFamily() objectcompare.Family {
	family := base(ClassifierServiceID, schema.ResourcePoolClassifierProperties, "resource pool classifier")
	family.Unsettled = func(*protocol.Envelope, *protocol.Envelope) error { return nil }
	family.Equal = func(current, desired *protocol.Envelope) (bool, error) {
		before, err := schema.WorkloadClassifier(current)
		if err != nil {
			return false, err
		}
		after, err := schema.WorkloadClassifier(desired)
		if err != nil {
			return false, err
		}
		return before == after, nil
	}
	family.Change = schema.CaptureResourcePoolClassifierChange
	return family
}

func base(service, kind, noun string) objectcompare.Family {
	return objectcompare.Family{
		ServiceID: service, Kind: kind, Capability: workload.CapabilityResourcePools, Noun: noun,
		ChangeCapability: true, KeepOmitted: true, OmittedRemoval: schema.ExplicitWorkloadRemoval(), DatabaseWide: true,
		DesiredModel: func() *protocol.ValueModel { return schema.NativeDesiredModel(kind) },
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.WorkloadBoundOwner(kind, subject)
		},
		NormalizeDesired: schema.NormalizeDesired, NormalizeObserved: schema.Normalize,
		Reserved: func(*protocol.ComparisonIdentity) string { return "" },
		// The kept object is the one the database holds, under the owner
		// the comparison reports.
		Preserve: func(_ *protocol.Reference, current *protocol.Envelope) (*protocol.Envelope, error) {
			return schema.ConvertNative(current, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
		},
	}
}

// unsettledPool leaves the pool `default` undecided until the read holds it:
// YDB creates it with the database, and Ptah changes its settings but never
// creates it.
func unsettledPool(current, desired *protocol.Envelope) error {
	if current == nil && desired != nil && desired.GetOwner().GetName() == workload.DefaultPool {
		return errDefaultUnread
	}
	return nil
}

type reason string

func (r reason) Error() string { return string(r) }

const errDefaultUnread reason = "the database's default pool must be inspected before changing its settings"
