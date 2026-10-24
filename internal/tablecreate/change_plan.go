package tablecreate

import (
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// ChangeKinds lists the comparison changes this service accepts. Each one
// reports that a table or a column of a table differs in a property the
// alteration reads from the captured table, so the alteration owns its plan.
func ChangeKinds() []string {
	return []string{schema.TablePropertiesChange, schema.ColumnStorageChange}
}

// validateChange re-normalizes a captured change and requires the host's receipt
// to match, so a forged change cannot reach the alteration.
func validateChange(change *protocol.PlanningChange) error {
	if err := provider.ValidateComparisonIdentity(change.GetSubject()); err != nil {
		return err
	}
	value := change.GetValue()
	normalize := schema.NormalizeTablePropertiesChange
	switch value.GetValue().GetKind() {
	case schema.TablePropertiesChange:
	case schema.ColumnStorageChange:
		normalize = schema.NormalizeColumnStorageChange
	default:
		return fmt.Errorf("YDB table planning received an unowned change")
	}
	expected, err := normalize(&protocol.ValueInput{Id: value.GetId(), Representation: protocol.ValueInput_CHANGE, Value: value.GetValue()})
	if err != nil {
		return err
	}
	if !proto.Equal(expected, value) {
		return fmt.Errorf("YDB table change has a forged normalization receipt")
	}
	owner := value.GetValue().GetOwner()
	subject := change.GetSubject()
	if subject.GetKind() != owner.GetKind() || subject.GetDatabase().GetSource() != owner.GetDatabase() || subject.GetSchema().GetSource() != owner.GetSchema() ||
		subject.GetName().GetSource() != owner.GetName() {
		return fmt.Errorf("YDB table change has a different bound identity")
	}
	return nil
}

// changedTable returns the identity of the table a changed table or column
// belongs to.
func changedTable(subject *protocol.ComparisonIdentity) *protocol.ComparisonIdentity {
	if subject.GetKind() == "table" {
		return subject
	}
	table := proto.CloneOf(subject)
	table.Kind, table.Name, table.Parent = "table", proto.CloneOf(subject.GetParent()), nil
	return table
}

// changeIndex returns the position of the altered table a change belongs to, or
// -1 when the request captures no alteration of it.
func changeIndex(request *protocol.PlanFeaturesRequest, change *protocol.PlanningChange) int {
	table := changedTable(change.GetSubject())
	for i, candidate := range request.Tables {
		if candidate.Action == protocol.PlanningTable_ALTER && provider.CompareIdentities(candidate.Subject, table) == 0 {
			return i
		}
	}
	return -1
}

func refusedChange(index int, message string) *protocol.PlannedFeatures {
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE,
		Kind: schema.TablePropertiesChange, Message: message, Input: &protocol.PlanningDiagnostic_Change{Change: uint32(index)}}}}
}

// changeReceipt accounts for a change with the steps of the alteration it
// belongs to: the alteration derives its operations from the captured tables,
// and the change names the property that made the table differ.
func changeReceipt(change *protocol.PlanningChange, steps []*protocol.StepIdentity) *protocol.PlanningReceipt {
	strategy := "The table alteration sets or drops the TTL, changes the automatic partitioning settings and alters the column families and column placements through the native table service"
	if change.GetValue().GetValue().GetKind() == schema.ColumnStorageChange {
		strategy = "The column table alteration resets the TTL the table holds and sets the declared one with YQL's ALTER TABLE through the query service"
	}
	if len(steps) == 0 {
		strategy = "The captured table already satisfies the declaration, so the alteration has no step to carry out"
	}
	receipt := &protocol.PlanningReceipt{Subject: proto.CloneOf(change.GetSubject()), Kind: change.GetValue().GetValue().GetKind(), Strategy: strategy}
	for _, id := range steps {
		receipt.Steps = append(receipt.Steps, proto.CloneOf(id))
	}
	return receipt
}
