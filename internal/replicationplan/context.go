package replicationplan

import (
	"context"
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// assessReplications gives every replication of the assigned context a
// receipt, the steps of its change where it has one, and refuses a plan
// whose tables would reach a table a replication the plan keeps owns: a
// table created at the path of a live replica, which YDB keeps read-only
// while the replication runs, and a failed-over replica dropped while the
// schema keeps the replication that made it. A read leaves a live replica
// out of the tables, so only this assessment sees such a table.
func assessReplications(ctx context.Context, request *protocol.PlanFeaturesRequest, result *protocol.PlannedFeatures) (*protocol.PlannedFeatures, error) {
	for index, subject := range provider.PlanningObjectSubjects(request) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		receipt := &protocol.PlanningReceipt{Subject: proto.CloneOf(subject), Kind: schema.ReplicationProperties, Strategy: "Keep the replication and the replica tables it writes"}
		for _, changed := range result.Changes {
			if provider.CompareIdentities(changed.Subject, subject) == 0 {
				for _, step := range changed.Steps {
					receipt.Steps = append(receipt.Steps, proto.CloneOf(step))
				}
			}
		}
		reason, err := keptReplicaRefusal(request, subject)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE,
				Kind: schema.ReplicationProperties, Message: reason, Input: &protocol.PlanningDiagnostic_ContextObject{ContextObject: uint32(index)}}}}, nil // #nosec G115 -- bounded by the request's object count
		}
		result.Objects = append(result.Objects, receipt)
	}
	return result, nil
}

// keptReplicaRefusal says why the plan's tables break the replication
// subject names, as it holds it now, or "". A replication the plan drops is
// the drop's to refuse.
func keptReplicaRefusal(request *protocol.PlanFeaturesRequest, subject *protocol.ComparisonIdentity) (string, error) {
	current := record(request.GetCurrentObjects(), subject)
	if current == nil || dropped(request, subject) {
		return "", nil
	}
	spec, state, err := schema.ObservedReplication(current.GetValue().GetValue())
	if err != nil {
		return "", err
	}
	name := subject.GetName().GetNormalized()
	if directory := subject.GetSchema().GetNormalized(); directory != "" {
		name = directory + "/" + name
	}
	targets := replication.Targets(spec)
	for _, table := range request.GetTables() {
		tablePath := tablePath(table.Subject)
		if !replication.UnderTarget(tablePath, targets) {
			continue
		}
		creates := table.GetAction() == protocol.PlanningTable_CREATE || table.GetAction() == protocol.PlanningTable_REBUILD
		switch {
		case state != replication.StateDone && creates:
			return fmt.Sprintf("table %s: it is a replica table async replication %s writes, read-only while the replication runs "+
				"(`path is an async replica table`); remove the table from the schema, or fail the replication over first with "+
				"ALTER ASYNC REPLICATION %s SET (STATE = 'DONE', FAILOVER_MODE = 'FORCE'), which makes it an ordinary table",
				tablePath, name, replication.Path(name)), nil
		case state == replication.StateDone && table.GetAction() == protocol.PlanningTable_DROP && record(request.GetDesiredObjects(), subject) != nil:
			return fmt.Sprintf("table %s: it is a table async replication %s created and failed over, which the schema keeps; "+
				"declare the table, which is an ordinary table now, or remove the replication from the schema", tablePath, name), nil
		}
	}
	return "", nil
}

// dropped reports whether the request's changes drop the replication
// subject names.
func dropped(request *protocol.PlanFeaturesRequest, subject *protocol.ComparisonIdentity) bool {
	for _, change := range request.GetChanges() {
		if !sameObject(change.Subject, subject) {
			continue
		}
		_, after, err := schema.ReplicationOperands(change.GetValue().GetValue())
		return err == nil && after == nil
	}
	return false
}

func record(state *protocol.ComparisonState, subject *protocol.ComparisonIdentity) *protocol.ComparisonRecord {
	for _, candidate := range state.GetRecords() {
		if candidate.GetKind() == schema.ReplicationProperties && sameObject(candidate.Subject, subject) {
			return candidate
		}
	}
	return nil
}

// sameObject reports whether two identities name one replication. The host
// names a declared replication without its database and a read one with the
// session's, and a plan runs in one database, so the database is not
// compared: compared, the declaration of a replication the plan keeps is
// never found beside the read one, and the plan drops a failed-over replica
// the schema leaves out, measured on 25.1.4.7 and 26.2.1.14.
func sameObject(a, b *protocol.ComparisonIdentity) bool {
	a, b = proto.CloneOf(a), proto.CloneOf(b)
	a.Database, b.Database = &protocol.ComparisonPart{}, &protocol.ComparisonPart{}
	return provider.CompareIdentities(a, b) == 0
}
