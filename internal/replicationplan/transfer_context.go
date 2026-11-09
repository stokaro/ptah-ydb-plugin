package replicationplan

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// assessTransfers acknowledges each transfer assigned as context and refuses
// a rebuild that would swap a table from under a transfer the plan keeps.
func assessTransfers(ctx context.Context, request *protocol.PlanFeaturesRequest, result *protocol.PlannedFeatures) (*protocol.PlannedFeatures, error) {
	for index, subject := range provider.PlanningObjectSubjects(request) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		receipt := &protocol.PlanningReceipt{Subject: proto.CloneOf(subject), Kind: schema.TransferProperties,
			Strategy: "Keep the transfer's target table and source changefeed while the transfer runs"}
		for _, changed := range result.Changes {
			if sameObject(changed.Subject, subject) {
				for _, step := range changed.Steps {
					receipt.Steps = append(receipt.Steps, proto.CloneOf(step))
				}
			}
		}
		reason, err := keptTransferRebuildRefusal(request, subject)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{
				Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, Kind: schema.TransferProperties, Message: reason,
				Input: &protocol.PlanningDiagnostic_ContextObject{ContextObject: uint32(index)}, // #nosec G115 -- bounded by the request's object count
			}}}, nil
		}
		result.Objects = append(result.Objects, receipt)
	}
	return result, nil
}

func keptTransferRebuildRefusal(request *protocol.PlanFeaturesRequest, subject *protocol.ComparisonIdentity) (string, error) {
	var current *protocol.ComparisonRecord
	for _, candidate := range request.GetCurrentObjects().GetRecords() {
		if candidate.GetKind() == schema.TransferProperties && sameObject(candidate.Subject, subject) {
			current = candidate
			break
		}
	}
	if current == nil {
		return "", nil
	}
	for _, change := range request.GetChanges() {
		if !sameObject(change.Subject, subject) {
			continue
		}
		_, after, err := schema.TransferOperands(change.GetValue().GetValue())
		if err != nil {
			return "", err
		}
		if after == nil {
			return "", nil
		}
	}
	spec, _, err := schema.ObservedTransfer(current.GetValue().GetValue())
	if err != nil {
		return "", err
	}
	effects := transferPaths(request.GetDatabasePath(), spec, topicByPath)
	for _, table := range request.GetTables() {
		if table.GetAction() != protocol.PlanningTable_REBUILD {
			continue
		}
		name := tablePath(table.Subject)
		for _, effect := range effects {
			if effect.Subject == nil && replication.TablePath(effect.Schema, effect.Name) == name ||
				effect.Subject.GetKind() == schema.ChangefeedEffectKind &&
					replication.TablePath(effect.Subject.GetSchema().GetNormalized(), effect.Subject.GetParent().GetNormalized()) == name {
				return fmt.Sprintf("rebuilding table %q: transfer %s writes the table or reads one of its changefeeds, "+
					"and a rebuild swaps the table from under it; drop the transfer, rebuild, and create it again",
					name, subject.GetName().GetNormalized()), nil
			}
		}
	}
	return "", nil
}
