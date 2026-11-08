// Package replicationplan lowers captured async replication and transfer
// changes into the host's shared graph: one step per object, refused where
// YDB cannot make the change in the state the object reported, where the
// target lacks a capability the declaration needs, or where the plan's
// tables would break what a replication owns.
//
// A replication creates its replica tables at its targets, and a drop with
// CASCADE drops them, so each step names those paths beside its own and is
// ordered against the common steps that act on them. A replication of this
// database reads its source tables, so it is created after a common step that
// creates one and dropped before one that drops it. A transfer reads its
// table and a topic or a changefeed of this database, so its step is ordered
// after a step that creates or changes either, and its drop before one that
// drops either. A creation or a change of either kind reads the secret its
// connection names by path, so it follows the secret's creation.
package replicationplan

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
		// The YDB engine built into the host at 74de0b65b created an async
		// replication after the tables and before the grants, and dropped
		// one ahead of the topics and tables, on 25.1.4.7 and 26.2.1.14.
		LateCreations: true,
		Strategy:      "Create the replication with its replica tables, point a paused one at the declared connection, or drop it with CASCADE unless it was failed over",
		Owner: func(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
			return schema.NativeBoundOwner(schema.ReplicationProperties, subject)
		},
		NormalizeChange: schema.NormalizeReplicationChange,
		Operation:       schema.ReplicationOperationFromChange,
		OperationIn:     replicationOperation,
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
		Paths: func(request *protocol.PlanFeaturesRequest, operation *protocol.Envelope) ([]objectplan.PathEffect, error) {
			step, err := schema.ReplicationStepOf(schema.ReplicationOperationChange(operation))
			if err != nil {
				return nil, err
			}
			root := request.GetDatabasePath()
			switch {
			case step.After == nil && step.Cascade():
				return append(targetPaths(step.Before.Items, protocol.PlanningEffect_DROP), sourcePaths(root, *step.Before)...), nil
			case step.After == nil:
				return sourcePaths(root, *step.Before), nil
			}
			secrets, err := secretReads(root, step.After.Connection)
			if err != nil || step.Before != nil {
				return secrets, err
			}
			return append(append(secrets, targetPaths(step.After.Items, protocol.PlanningEffect_CREATE)...), sourcePaths(root, *step.After)...), nil
		},
	}
}

// replicationOperation lowers a replication change to the operation that runs
// it. A creation whose replica table a common step of the plan reads, such as
// a view over it, waits for its replica tables before the plan goes on: YDB
// creates them some time after CREATE ASYNC REPLICATION returns, and refuses
// the view until then (SCHEME_ERROR, issue 2003, on 25.1.4.7 and 26.2.1.14).
// Every other change goes on at once.
func replicationOperation(request *protocol.PlanFeaturesRequest, id string, change *protocol.Envelope) (*protocol.NormalizedValue, error) {
	step, err := schema.ReplicationStepOf(change)
	if err != nil {
		return nil, err
	}
	operation := schema.ReplicationRun
	if step.Before == nil && step.After != nil && slices.ContainsFunc(targetPaths(step.After.Items, protocol.PlanningEffect_CREATE), func(target objectplan.PathEffect) bool {
		return objectplan.ReadByCommonStep(request, target.Schema, target.Name)
	}) {
		operation = schema.ReplicationAwaitReplicas
	}
	return schema.ReplicationOperationAs(id, change, operation)
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
			if step.After == nil {
				return transferPaths(request.GetDatabasePath(), *step.Before, topicByIdentity), nil
			}
			secrets, err := secretReads(request.GetDatabasePath(), step.After.Connection)
			if err != nil {
				return nil, err
			}
			return append(secrets, transferPaths(request.GetDatabasePath(), *step.After, topicByPath)...), nil
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

// secretReads is a read of each scheme secret connection names by path, as a
// statement that creates or changes a replication or a transfer reads it: YDB
// looks the secret up when it creates the object or changes its connection
// (`secret ... not found`), so whatever creates the secret runs first. Each secret is read by
// the identity the secret planner names it by, and by its scheme path. root
// is the database the object runs in; a path outside it is refused, in the
// words of the host's embedded engine.
func secretReads(root string, connection replication.Connection) ([]objectplan.PathEffect, error) {
	paths, err := replication.SecretPaths(root, connection)
	if err != nil {
		return nil, err
	}
	var effects []objectplan.PathEffect
	for _, secret := range paths {
		effects = append(effects, objectplan.PathEffect{Subject: secretSubject(secret.Schema, secret.Name), Action: protocol.PlanningEffect_READ},
			objectplan.PathEffect{Schema: secret.Schema, Name: secret.Name, Action: protocol.PlanningEffect_READ})
	}
	return effects, nil
}

// secretSubject is the secret at directory and name as the secret planner
// names one it creates: in no database, as the host names a secret it
// compares.
func secretSubject(directory, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: schema.SecretProperties, Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{}, Schema: part(directory),
		Parent: &protocol.ComparisonPart{}, Name: part(name)}
}

// targetPaths is action on the scheme path of each item's target.
func targetPaths(items []replication.Item, action protocol.PlanningEffect_Action) []objectplan.PathEffect {
	var effects []objectplan.PathEffect
	seen := make(map[string]bool)
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

// sourcePaths is a read of each path a replication of the database at root
// replicates, as the path below root: the step that creates the replication
// comes after a common step that creates or changes the table, and the step
// that drops it before a common step that drops it. A replication created
// before its source table stays without its replica, measured on 25.1.4.7 and
// 26.2.1.14: the replication reads as running with its item, and YDB creates
// no replica table for a minute and more. A replication of another database
// reads no path here.
func sourcePaths(root string, spec replication.ReplicationSpec) []objectplan.PathEffect {
	root = strings.TrimRight(root, "/")
	if database := strings.TrimRight(replication.ConnectionDatabase(spec.Connection.ConnectionString), "/"); root == "" || database != root {
		return nil
	}
	var effects []objectplan.PathEffect
	seen := make(map[string]bool)
	for _, item := range spec.Items {
		source := replication.SourceKey(item.Source, root)
		if strings.HasPrefix(source, "/") || seen[source] {
			continue
		}
		seen[source] = true
		directory, name := split(source)
		effects = append(effects, objectplan.PathEffect{Schema: directory, Name: name, Action: protocol.PlanningEffect_READ})
	}
	return effects
}

// transferPaths is a read of the table spec writes and of what it reads in
// the database at root: a standalone topic or a changefeed of a table at its
// source path. Only the plan knows which the path holds, so a transfer reads
// both, and the one nothing touches orders nothing.
//
// The changefeed is read by its own identity, the one the changefeed planner
// names, since the table's changefeeds are planned by another service than
// the transfer and no scheme path of a changefeed is named by the step that
// adds it: without it a transfer created beside its changefeed is created
// first, measured on 26.2.1.14. A transfer that is created or changed reads
// the topic by its path, so it comes after whatever creates or changes the
// object at the path. A transfer that is dropped reads the topic by the
// topic's own identity instead: a read of the path would read whatever the
// plan creates there once the topic is gone, such as a secret, and the host
// would then order the drop after that creation and after the topic's drop,
// which YDB runs under a transfer that still reads the topic. A source of
// another database names no path here. readTopic is the caller's choice of
// the two reads: [topicByIdentity] for a dropped transfer, [topicByPath]
// otherwise.
func transferPaths(root string, spec replication.TransferSpec, readTopic func(root, directory, name string) []objectplan.PathEffect) []objectplan.PathEffect {
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
	effects = append(effects, readTopic(root, directory, name)...)
	if directory != "" {
		schemaName, table := split(directory)
		effects = append(effects, objectplan.PathEffect{Subject: changefeedSubject(schemaName, table, name), Action: protocol.PlanningEffect_READ})
	}
	return effects
}

// topicByPath is a read of the object at directory and name by its path, as a
// transfer that is created or changed reads its topic.
func topicByPath(_, directory, name string) []objectplan.PathEffect {
	return []objectplan.PathEffect{{Schema: directory, Name: name, Action: protocol.PlanningEffect_READ}}
}

// topicByIdentity is a read of the topic at directory and name by its own
// identity, in no database and in the database at root when root is set, as
// a dropped transfer reads its topic.
func topicByIdentity(root, directory, name string) []objectplan.PathEffect {
	databases := []string{""}
	if root != "" {
		databases = append(databases, root)
	}
	effects := make([]objectplan.PathEffect, 0, len(databases))
	for _, database := range databases {
		effects = append(effects, objectplan.PathEffect{Subject: topicSubject(database, directory, name), Action: protocol.PlanningEffect_READ})
	}
	return effects
}

// topicSubject is the topic at directory and name in database, as a step
// that changes the topic names it: in no database, as the host names a topic
// it compares, or in the session's, as a read names it. A topic step names one
// of the two, so a dropped transfer reads both.
func topicSubject(database, directory, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: schema.TopicProperties, Database: part(database), Catalog: &protocol.ComparisonPart{}, Schema: part(directory),
		Parent: &protocol.ComparisonPart{}, Name: part(name)}
}

// changefeedSubject is changefeed name of the table at directory and table as
// the changefeed planner names it: under the table, which the host names
// without its database.
func changefeedSubject(directory, table, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: schema.ChangefeedEffectKind, Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
		Schema: part(directory), Parent: part(table), Name: part(name)}
}

func part(value string) *protocol.ComparisonPart {
	return &protocol.ComparisonPart{Source: value, Normalized: value}
}

func split(relative string) (directory, name string) {
	cleaned := path.Clean(strings.Trim(relative, "/"))
	directory, name = path.Split(cleaned)
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
