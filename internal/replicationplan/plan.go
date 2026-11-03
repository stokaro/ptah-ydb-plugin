// Package replicationplan lowers captured async replication and transfer
// changes into the host's shared graph: one step per object, refused where
// YDB cannot make the change in the state the object reported, where the
// target lacks a capability the declaration needs, or where the plan's
// tables would break what a replication owns.
//
// A replication creates its replica tables at its targets, and a drop with
// CASCADE drops them, so each step names those paths beside its own and is
// ordered against the common steps that act on them. A transfer reads its
// table and a topic of this database, so its step is ordered after a common
// step that creates or changes either, and its drop before one that drops
// either.
package replicationplan

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The planning services this package owns.
const (
	// ReplicationServiceID names the async replication planner.
	ReplicationServiceID = "ydb-async-replications"
	// TransferServiceID names the transfer planner.
	TransferServiceID = "ydb-transfers"
)

// PlanReplications contributes one step per replication change and, where
// the host assigns replications for assessment, a receipt for each
// replication of either side.
func PlanReplications(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	result, err := objectplan.Plan(ctx, request, replicationFamily())
	if err != nil || len(result.GetDiagnostics()) != 0 || len(request.GetContextKinds()) == 0 {
		return result, err
	}
	return assessReplications(ctx, request, result)
}

func replicationFamily() objectplan.Family {
	return objectplan.Family{
		ServiceID: ReplicationServiceID, Kind: schema.ReplicationProperties, ChangeKind: schema.ReplicationChange,
		Capability: replication.CapabilityReplication, Noun: "async replication", StepPrefix: "replication",
		Strategy: "Create the replication with its replica tables, point a paused one at the declared connection, or drop it with CASCADE unless it was failed over",
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.ReplicationProperties, subject)
		},
		NormalizeChange: schema.NormalizeReplicationChange,
		Operation:       schema.ReplicationOperationFromChange,
		Present: func(change *protocol.Envelope) (bool, bool, error) {
			before, after, err := schema.ReplicationOperands(change)
			return before != nil, after != nil, err
		},
		Refuse: func(change *protocol.Envelope, capabilities map[string]bool) error {
			step, err := schema.ReplicationStepOf(change)
			if err != nil {
				return err
			}
			if refusal := replication.RefuseReplication(display(change.GetOwner()), step.Before, step.After, step.State, capabilities); refusal != nil {
				return refusal
			}
			return nil
		},
		RefuseInPlan: refuseReplicaTables,
		Paths: func(_ *protocol.PlanFeaturesRequest, operation *protocol.Envelope) ([]objectplan.PathEffect, error) {
			step, err := schema.ReplicationStepOf(schema.ReplicationOperationChange(operation))
			if err != nil {
				return nil, err
			}
			switch {
			case step.Before == nil:
				return targetPaths(step.After.Items, protocol.PlanningEffect_CREATE), nil
			case step.After == nil && step.Cascade():
				return targetPaths(step.Before.Items, protocol.PlanningEffect_DROP), nil
			}
			return nil, nil
		},
	}
}

// PlanTransfers contributes one step per transfer change.
func PlanTransfers(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	return objectplan.Plan(ctx, request, objectplan.Family{
		ServiceID: TransferServiceID, Kind: schema.TransferProperties, ChangeKind: schema.TransferChange,
		Capability: replication.CapabilityTransfers, Noun: "transfer", StepPrefix: "transfer",
		Strategy: "Create the transfer, change its lambda, batch settings or connection in place, or drop it with the consumer YDB created for it",
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.TransferProperties, subject)
		},
		NormalizeChange: schema.NormalizeTransferChange,
		Operation:       schema.TransferOperationFromChange,
		Present: func(change *protocol.Envelope) (bool, bool, error) {
			before, after, err := schema.TransferOperands(change)
			return before != nil, after != nil, err
		},
		Refuse: func(change *protocol.Envelope, capabilities map[string]bool) error {
			step, err := schema.TransferStepOf(change)
			if err != nil {
				return err
			}
			if refusal := replication.RefuseTransfer(display(change.GetOwner()), step.Before, step.After, step.State, capabilities); refusal != nil {
				return refusal
			}
			return nil
		},
		Paths: func(request *protocol.PlanFeaturesRequest, operation *protocol.Envelope) ([]objectplan.PathEffect, error) {
			step, err := schema.TransferStepOf(schema.TransferOperationChange(operation))
			if err != nil {
				return nil, err
			}
			spec := step.After
			if spec == nil {
				spec = step.Before
			}
			return transferPaths(request.GetDatabasePath(), *spec), nil
		},
	})
}

// refuseReplicaTables refuses a replication change the plan's tables would
// break, as the host's embedded engine refuses it:
//
//   - a drop with CASCADE while the plan keeps or creates a table under one
//     of its targets, since the drop takes that table along, and a drop
//     without it leaves the table read-only for good;
//   - a creation where the plan keeps or creates a table at one of its
//     targets, since YDB creates a replica only at a free path (measured:
//     `Create dst error`).
func refuseReplicaTables(request *protocol.PlanFeaturesRequest, change *protocol.Envelope) error {
	step, err := schema.ReplicationStepOf(change)
	if err != nil {
		return err
	}
	name := display(change.GetOwner())
	switch {
	case step.After == nil && step.Cascade():
		targets := replication.Targets(*step.Before)
		for _, table := range request.Tables {
			if table.GetDesired() == nil || !replication.UnderTarget(tablePath(table.Subject), targets) {
				continue
			}
			return &replication.Refusal{Subject: "async replication " + name, Reason: fmt.Sprintf("the schema drops it and declares table %s, "+
				"its replica, and YDB keeps a replica of a replication dropped without failover read-only for good "+
				"(`path is an async replica table`); fail it over first with ALTER ASYNC REPLICATION %s SET "+
				"(STATE = 'DONE', FAILOVER_MODE = 'FORCE'), which makes its tables ordinary, and plan again",
				tablePath(table.Subject), replication.Path(name))}
		}
	case step.Before == nil:
		for _, target := range replication.Targets(*step.After) {
			for _, table := range request.Tables {
				if (table.GetDesired() == nil && table.GetCurrent() == nil) || table.GetAction() == protocol.PlanningTable_DROP ||
					!replication.UnderTarget(tablePath(table.Subject), []string{target}) {
					continue
				}
				return &replication.Refusal{Subject: "async replication " + name, Reason: fmt.Sprintf("its target %s holds table %s, "+
					"and YDB creates a replica only at a free path (measured: `Create dst error`)", target, tablePath(table.Subject))}
			}
		}
	}
	return nil
}

// targetPaths is action on the scheme path of each item's target.
func targetPaths(items []replication.Item, action protocol.PlanningEffect_Action) []objectplan.PathEffect {
	var effects []objectplan.PathEffect
	seen := map[string]bool{}
	for _, target := range replication.Targets(replication.ReplicationSpec{Items: items}) {
		if seen[target] {
			continue
		}
		seen[target] = true
		directory, name := split(target)
		effects = append(effects, objectplan.PathEffect{Schema: directory, Name: name, Action: action})
	}
	return effects
}

// transferPaths is a read of the table spec writes and of the topic of the
// database at root it reads. A source of another database names no path
// here.
func transferPaths(root string, spec replication.TransferSpec) []objectplan.PathEffect {
	var effects []objectplan.PathEffect
	directory, name := split(spec.Target)
	effects = append(effects, objectplan.PathEffect{Schema: directory, Name: name, Action: protocol.PlanningEffect_READ})
	if !replication.LocalSource(spec) {
		return effects
	}
	source := spec.Source
	if rest, under := strings.CutPrefix(source, strings.TrimRight(root, "/")+"/"); root != "" && under {
		source = rest
	}
	if strings.HasPrefix(source, "/") {
		return effects
	}
	directory, name = split(source)
	return append(effects, objectplan.PathEffect{Schema: directory, Name: name, Action: protocol.PlanningEffect_READ})
}

func split(relative string) (string, string) {
	cleaned := path.Clean(strings.Trim(relative, "/"))
	directory, name := path.Split(cleaned)
	return strings.TrimSuffix(directory, "/"), name
}

func tablePath(subject *protocol.ComparisonIdentity) string {
	return replication.TablePath(subject.GetSchema().GetNormalized(), subject.GetName().GetNormalized())
}

func display(owner *protocol.Reference) string {
	if owner.GetSchema() == "" {
		return owner.GetName()
	}
	return owner.GetSchema() + "/" + owner.GetName()
}
