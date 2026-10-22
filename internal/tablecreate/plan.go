package tablecreate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// ServiceID names the row-table lifecycle assessment.
const ServiceID = "ydb-row-table-create"

// ParentKinds assigns the native table and column settings to this assessment.
func ParentKinds() []string { return []string{schema.TableProperties, schema.ColumnProperties} }

type creation struct {
	index     int
	table     *protocol.PlanningTable
	operation *protocol.NormalizedValue
	id        *protocol.StepIdentity
}

// Plan contributes native table creation, and the alteration of an existing
// table's column families, to the common graph. Dropping or rebuilding a table
// remains an explicit refusal until its complete state is handled. Every other
// setting of an altered table, and its column additions and removals, stay with
// the host's steps.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.PlanningId != ServiceID || len(request.Changes) != 0 {
		return nil, fmt.Errorf("invalid YDB table creation assignment")
	}
	if err := validateTableIdentifiers(request.Identifiers); err != nil {
		return nil, err
	}
	result := &protocol.PlannedFeatures{Complete: true}
	var creations []creation
	var alterations []alteration
	for i, table := range request.Tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if table.Action == protocol.PlanningTable_CONTEXT {
			continue
		}
		if !slices.Equal(request.ParentKinds, ParentKinds()) {
			return nil, fmt.Errorf("invalid YDB table parent assignment")
		}
		if table.Action != protocol.PlanningTable_CREATE && table.Action != protocol.PlanningTable_ALTER {
			return refused(i, "Only native row-table creation and column family alteration are implemented"), nil
		}
		transactional, known := request.Capabilities["transactional_ddl"]
		if !known || transactional {
			return refused(i, "YDB table creation requires established nontransactional DDL"), nil
		}
		owner, err := boundOwner(table.Subject)
		if err != nil {
			return nil, err
		}
		if request.DatabasePath != "" && owner.Database != "" && request.DatabasePath != owner.Database {
			return nil, fmt.Errorf("YDB table creation targets another database")
		}
		if table.Action == protocol.PlanningTable_ALTER {
			change, reason, err := planAlteration(owner, table)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				return refused(i, reason), nil
			}
			item := alteration{index: i, table: table, change: change}
			if !change.empty() {
				if item.operation, err = CaptureAlteration("pending", owner, change); err != nil {
					return refused(i, err.Error()), nil
				}
			}
			alterations = append(alterations, item)
			continue
		}
		if table.Current != nil {
			return nil, fmt.Errorf("YDB table creation requires established absence")
		}
		operation, err := Capture("pending", owner, table.Desired)
		if err != nil {
			return refused(i, err.Error()), nil
		}
		creations = append(creations, creation{index: i, table: table, operation: operation})
	}
	slices.SortFunc(creations, func(a, b creation) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	contribution := &protocol.PlanningContribution{Owner: schema.ProviderID}
	receiptSteps := make(map[int]*protocol.StepIdentity)
	for i := range creations {
		item := &creations[i]
		item.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-create/%06d", i)}
		item.operation.Id = item.id.Name
		for _, other := range request.Tables {
			if provider.CompareIdentities(other.Subject, item.table.Subject) == 0 {
				continue
			}
			if err := disjoint(item.table.Subject, other.Subject); err != nil {
				return refused(item.index, err.Error()), nil
			}
		}
		facts := item.operation.OperationSemantics
		slot := proto.CloneOf(item.table.Subject)
		slot.Kind = "ydb.scheme-path"
		contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: item.id,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.operation},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(item.table.Subject), Action: protocol.PlanningEffect_CREATE},
					{Subject: slot, Action: protocol.PlanningEffect_CREATE}}}})
		receiptSteps[item.index] = item.id
	}
	slices.SortFunc(alterations, func(a, b alteration) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	number := 0
	for i := range alterations {
		item := &alterations[i]
		if item.operation == nil {
			continue
		}
		item.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-alter/%06d", number)}
		number++
		item.operation.Id = item.id.Name
		facts := item.operation.OperationSemantics
		families := proto.CloneOf(item.table.Subject)
		families.Kind = "ydb.column-families"
		contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: item.id,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.operation},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(item.table.Subject), Action: protocol.PlanningEffect_READ},
					{Subject: families, Action: protocol.PlanningEffect_ALTER}}}})
		contribution.Dependencies = append(contribution.Dependencies, alterStepDependencies(*item, request.CommonSteps)...)
		receiptSteps[item.index] = item.id
	}
	// This service can replace only a complete common table creation. A column
	// step or a mixed footprint needs its own lifecycle, never a partial rewrite.
	claimed := make(map[int]bool)
	for _, step := range request.CommonSteps {
		if slices.ContainsFunc(alterations, func(item alteration) bool { return provider.CompareIdentities(step.Parent, item.table.Subject) == 0 }) {
			// The host keeps the column and setting steps of a table this service only alters.
			continue
		}
		matched := -1
		for i, item := range creations {
			if provider.CompareIdentities(step.Parent, item.table.Subject) == 0 {
				matched = i
				break
			}
		}
		if matched < 0 {
			if len(creations) == 0 {
				return nil, fmt.Errorf("common steps have no captured table creation")
			}
			return refused(creations[0].index, "A common step is outside the supported table creation lifecycle"), nil
		}
		item := creations[matched]
		effects := step.GetMetadata().GetEffects()
		if claimed[matched] || step.AddedColumn != nil || len(effects) != 1 ||
			effects[0].Action != protocol.PlanningEffect_CREATE || provider.CompareIdentities(effects[0].Subject, item.table.Subject) != 0 {
			return refused(item.index, "A table creation rewrite requires one complete common table footprint"), nil
		}
		claimed[matched] = true
		result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: []*protocol.StepIdentity{proto.CloneOf(step.Id)}, Replacement: proto.CloneOf(item.id)})
	}
	for i, table := range request.Tables {
		if table.Action == protocol.PlanningTable_CONTEXT {
			continue
		}
		strategy := "Create the complete captured row table through the native table service"
		var steps []*protocol.StepIdentity
		if id := receiptSteps[i]; id != nil {
			steps = []*protocol.StepIdentity{proto.CloneOf(id)}
		}
		if table.Action == protocol.PlanningTable_ALTER {
			strategy = "Alter the column families and their column placements through the native table service; other settings and column steps stay with the host"
			if steps == nil {
				strategy = "The column families and column placements already match the declaration"
			}
		}
		for _, kind := range request.ParentKinds {
			result.Parents = append(result.Parents, &protocol.PlanningParentReceipt{Action: table.Action, Receipt: &protocol.PlanningReceipt{
				Subject: proto.CloneOf(table.Subject), Kind: kind, Strategy: strategy, Steps: steps}})
		}
	}
	if len(contribution.Steps) > 0 {
		result.Contributions = []*protocol.PlanningContribution{contribution}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
func refused(index int, message string) *protocol.PlannedFeatures {
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{Code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE,
		Kind: schema.TableProperties, Message: message, Input: &protocol.PlanningDiagnostic_Parent{Parent: uint32(index)}}}}
}
func boundOwner(subject *protocol.ComparisonIdentity) (*protocol.Reference, error) {
	if err := provider.ValidateComparisonIdentity(subject); err != nil {
		return nil, err
	}
	if subject.Kind != "table" || subject.GetParent().GetSource() != "" || subject.GetCatalog().GetSource() != "" ||
		len(subject.Path)+len(subject.Signature) != 0 {
		return nil, fmt.Errorf("invalid YDB table planning identity")
	}
	for _, part := range []*protocol.ComparisonPart{subject.Database, subject.Catalog, subject.Schema, subject.Name, subject.Parent} {
		if part.GetSource() != part.GetNormalized() {
			return nil, fmt.Errorf("YDB table identifiers must retain exact spelling")
		}
	}
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: subject.GetDatabase().GetNormalized(), Schema: subject.GetSchema().GetNormalized(), Name: subject.GetName().GetNormalized()}
	return owner, validateOwner(owner)
}
func disjoint(a, b *protocol.ComparisonIdentity) error {
	first, err := boundOwner(a)
	if err != nil {
		return err
	}
	second, err := boundOwner(b)
	if err != nil {
		return err
	}
	if (first.Database == "") != (second.Database == "") {
		return fmt.Errorf("YDB table context has an unresolved database binding")
	}
	if first.Database != second.Database {
		return nil
	}
	path := func(o *protocol.Reference) string {
		if o.Schema != "" {
			return o.Schema + "/" + o.Name
		}
		return o.Name
	}
	x, y := path(first), path(second)
	if strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/") || x == y {
		return fmt.Errorf("YDB table paths overlap without a captured directory lifecycle")
	}
	return nil
}
