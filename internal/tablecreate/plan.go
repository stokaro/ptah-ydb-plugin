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

// ParentKinds assigns the native table and column settings, the storage of a
// column table, and the changefeeds of a table to this assessment: the host
// plans no table whose facets no planning service claims, and the comparison
// reports no changefeed change, so an alteration keeps them as they are.
func ParentKinds() []string {
	return []string{schema.TableProperties, schema.ColumnProperties, schema.IndexProperties, schema.ChangefeedProperties, schema.ColumnStorageProperties}
}

type creation struct {
	index     int
	table     *protocol.PlanningTable
	operation *protocol.NormalizedValue
	id        *protocol.StepIdentity
}

// removal is a table the plan drops, and the host's steps its drop replaces.
type removal struct {
	index     int
	table     *protocol.PlanningTable
	operation *protocol.NormalizedValue
	id        *protocol.StepIdentity
	sources   []*protocol.StepIdentity
}

// Plan contributes native table creation, the alteration of an existing
// table's column families, columns and TTL, and the removal of a table, to
// the common graph. The host's steps that add a nullable column or drop a
// column are replaced by native operations, ordered with the family operation
// in one graph, and the host's steps that drop a table by its removal.
// Rebuilding a table remains an explicit refusal until its complete state is
// handled. Every other setting of an altered table, and a column step the
// native call cannot state, stay with the host's steps.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.PlanningId != ServiceID {
		return nil, fmt.Errorf("invalid YDB table creation assignment")
	}
	for _, change := range request.Changes {
		if err := validateChange(change); err != nil {
			return nil, err
		}
	}
	if err := validateTableIdentifiers(request.Identifiers); err != nil {
		return nil, err
	}
	result := &protocol.PlannedFeatures{Complete: true}
	var creations []creation
	var drops []removal
	var rebuilds []*rebuild
	names := requestTableNames(request)
	var alterations []alteration
	// columnAlterations are the column tables the request alters. Each holds
	// what the declaration asks for, or differs from it in its TTL alone, since
	// every other change is refused. columnTTLs are the TTL changes.
	var columnAlterations []int
	var columnTTLs []columnTTLStep
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
		if table.Action == protocol.PlanningTable_REBUILD {
			owner, err := boundOwner(table.Subject)
			if err != nil {
				return nil, err
			}
			column, err := columnRebuild(owner, table)
			if err != nil {
				return nil, err
			}
			if column {
				return refused(i, fmt.Sprintf("table %q: %s", tableLabel(owner), columnRebuildRefusal)), nil
			}
			if !request.GetPolicy().GetAllowTableRebuild() {
				// The policy is a request: without it this provider rebuilds no
				// table, whatever the host proposes.
				return refused(i, fmt.Sprintf("rebuilding table %q, which Ptah plans when asked with %s", tableLabel(owner), requestedWith(request.GetPolicy()))), nil
			}
			desired, err := Compile(owner, table.Desired)
			if err != nil {
				return refused(i, fmt.Sprintf("table %q: %v", tableLabel(owner), err)), nil
			}
			item, reason, err := planRebuild(len(rebuilds), owner, table, desired, names)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				return refused(i, reason), nil
			}
			item.index = i
			rebuilds = append(rebuilds, item)
			continue
		}
		if table.Action == protocol.PlanningTable_DROP {
			transactional, known := request.Capabilities["transactional_ddl"]
			if !known || transactional {
				return refused(i, "YDB table removal requires established nontransactional DDL"), nil
			}
			owner, err := boundOwner(table.Subject)
			if err != nil {
				return nil, err
			}
			if table.Current == nil || len(table.Current.GetColumns()) == 0 {
				return refused(i, fmt.Sprintf("table %q: a removal checks the table it drops against the read of it, and the table was not read", tableLabel(owner))), nil
			}
			operation, err := CaptureDrop("pending", owner, table.Current)
			if err != nil {
				return refused(i, fmt.Sprintf("table %q: %v", tableLabel(owner), err)), nil
			}
			drops = append(drops, removal{index: i, table: table, operation: operation})
			continue
		}
		if table.Action != protocol.PlanningTable_CREATE && table.Action != protocol.PlanningTable_ALTER {
			return refused(i, "Only native row-table creation, alteration of column families, columns, the TTL, the partitioning settings and the indexes, column-table creation and table removal are implemented"), nil
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
			column, reason, columnTTL, err := planColumnAlteration(owner, table, request.CommonSteps, request.Capabilities)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				return refused(i, reason), nil
			}
			if column {
				columnAlterations = append(columnAlterations, i)
				if columnTTL != nil {
					columnTTLs = append(columnTTLs, columnTTLStep{index: i, table: table, owner: owner, change: *columnTTL})
				}
				continue
			}
			if needs, desired := rebuildNeeds(owner, table); len(needs) != 0 {
				if !request.GetPolicy().GetAllowTableRebuild() {
					return refused(i, rebuildRefusal(needs[0], request.GetPolicy())), nil
				}
				item, reason, err := planRebuild(len(rebuilds), owner, table, desired, names)
				if err != nil {
					return nil, err
				}
				if reason != "" {
					return refused(i, reason), nil
				}
				item.index = i
				rebuilds = append(rebuilds, item)
				continue
			}
			changes, reason, err := planAlteration(owner, table)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				return refused(i, reason), nil
			}
			change, ttl, partitioning := changes.family, changes.ttl, changes.partitioning
			item := alteration{index: i, table: table, change: change, ttl: ttl, partitioning: partitioning}
			for _, indexChange := range changes.indexes {
				step := &indexStep{change: indexChange}
				if step.operation, err = CaptureIndexAlteration("pending", owner, indexChange); err != nil {
					return refused(i, err.Error()), nil
				}
				item.indexes = append(item.indexes, step)
			}
			if !change.empty() {
				if item.operation, err = CaptureAlteration("pending", owner, change); err != nil {
					return refused(i, err.Error()), nil
				}
			}
			if !ttl.empty() {
				if item.ttlOperation, err = CaptureTTLAlteration("pending", owner, ttl); err != nil {
					return refused(i, err.Error()), nil
				}
			}
			if !partitioning.empty() {
				if item.partitioningOperation, err = CapturePartitioningAlteration("pending", owner, partitioning); err != nil {
					return refused(i, err.Error()), nil
				}
			}
			if item.claims, reason, err = planColumnClaims(owner, table, request.CommonSteps); err != nil {
				return nil, err
			}
			if reason != "" {
				return refused(i, reason), nil
			}
			for _, claim := range item.claims {
				if claim.operation, err = captureColumns("pending", owner, claim); err != nil {
					return refused(i, err.Error()), nil
				}
			}
			alterations = append(alterations, item)
			continue
		}
		if table.Current != nil {
			return nil, fmt.Errorf("YDB table creation requires established absence")
		}
		capture := Capture
		if IsColumnTable(table.Desired) {
			capture = CaptureColumnTable
		}
		operation, err := capture("pending", owner, table.Desired)
		if err != nil {
			return refused(i, err.Error()), nil
		}
		if isColumnCreation(operation) {
			created, err := ColumnTable(operation.Value)
			if err != nil {
				return nil, err
			}
			if reason := tieredTTLRefusal(request.Capabilities, fmt.Sprintf("column table %q", tableLabel(owner)), created.Ttl); reason != "" {
				return refused(i, reason), nil
			}
		}
		creations = append(creations, creation{index: i, table: table, operation: operation})
	}
	slices.SortFunc(creations, func(a, b creation) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	contribution := &protocol.PlanningContribution{Owner: schema.ProviderID}
	receiptSteps := make(map[int][]*protocol.StepIdentity)
	var rowCreations, columnCreations int
	for i := range creations {
		item := &creations[i]
		prefix, number := "row-table-create", &rowCreations
		if isColumnCreation(item.operation) {
			prefix, number = "column-table-create", &columnCreations
		}
		item.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", prefix, *number)}
		*number++
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
		effects := []*protocol.PlanningEffect{{Subject: proto.CloneOf(item.table.Subject), Action: protocol.PlanningEffect_CREATE},
			{Subject: slot, Action: protocol.PlanningEffect_CREATE}}
		if !isColumnCreation(item.operation) {
			created, err := Request(item.operation.Value)
			if err != nil {
				return nil, err
			}
			for _, index := range created.GetIndexes() {
				effects = append(effects, &protocol.PlanningEffect{Subject: indexSubject(item.table.Subject, index.GetName()), Action: protocol.PlanningEffect_CREATE})
			}
		}
		contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: item.id,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.operation},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
				Effects: effects}})
		receiptSteps[item.index] = []*protocol.StepIdentity{item.id}
	}
	slices.SortFunc(drops, func(a, b removal) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	for number := range drops {
		item := &drops[number]
		item.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("table-drop/%06d", number)}
		item.operation.Id = item.id.Name
		facts := item.operation.OperationSemantics
		slot := proto.CloneOf(item.table.Subject)
		slot.Kind = "ydb.scheme-path"
		contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: item.id,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.operation},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(item.table.Subject), Action: protocol.PlanningEffect_DROP},
					{Subject: slot, Action: protocol.PlanningEffect_DROP}}}})
		receiptSteps[item.index] = []*protocol.StepIdentity{item.id}
	}
	for _, item := range rebuilds {
		var ids []*protocol.StepIdentity
		for n, step := range item.steps {
			contribution.Steps = append(contribution.Steps, step)
			ids = append(ids, proto.CloneOf(step.Id))
			if n > 0 {
				contribution.Dependencies = append(contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(item.steps[n-1].Id), After: proto.CloneOf(step.Id)})
			}
		}
		receiptSteps[item.index] = ids
	}
	slices.SortFunc(alterations, func(a, b alteration) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	var families, additions, removals, ttls, partitionings, indexAdditions, indexRemovals int
	for i := range alterations {
		item := &alterations[i]
		subject := item.table.Subject
		var added, dropped []*protocol.StepIdentity
		for _, claim := range item.claims {
			prefix, number := "row-table-drop-column", &removals
			if claim.add {
				prefix, number = "row-table-add-column", &additions
			}
			claim.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", prefix, *number)}
			*number++
			claim.operation.Id = claim.id.Name
			facts := claim.operation.OperationSemantics
			contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: claim.id,
				Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: claim.operation},
				Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
					Placement: claim.source.GetMetadata().GetPlacement(), Effects: cloneEffects(claim.source.GetMetadata().GetEffects())}})
			result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: []*protocol.StepIdentity{proto.CloneOf(claim.source.Id)}, Replacement: proto.CloneOf(claim.id)})
			if claim.add {
				added = append(added, claim.id)
			} else {
				dropped = append(dropped, claim.id)
			}
		}
		var ordered []*protocol.StepIdentity
		ordered = append(ordered, added...)
		if item.operation != nil {
			item.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-alter/%06d", families)}
			families++
			item.operation.Id = item.id.Name
			facts := item.operation.OperationSemantics
			familySlot := proto.CloneOf(subject)
			familySlot.Kind = "ydb.column-families"
			contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: item.id,
				Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.operation},
				Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
					Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ},
						{Subject: familySlot, Action: protocol.PlanningEffect_ALTER}}}})
			contribution.Dependencies = append(contribution.Dependencies, alterStepDependencies(*item, request.CommonSteps)...)
			ordered = append(ordered, item.id)
		}
		if item.ttlOperation != nil {
			item.ttlID = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-ttl/%06d", ttls)}
			ttls++
			item.ttlOperation.Id = item.ttlID.Name
			facts := item.ttlOperation.OperationSemantics
			ttlSlot := proto.CloneOf(subject)
			ttlSlot.Kind = "ydb.ttl"
			contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: item.ttlID,
				Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.ttlOperation},
				Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
					Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ},
						{Subject: ttlSlot, Action: ttlAction(item.ttl)}}}})
			contribution.Dependencies = append(contribution.Dependencies, ttlStepDependencies(*item, request.CommonSteps)...)
			ordered = append(ordered, item.ttlID)
		}
		if item.partitioningOperation != nil {
			item.partitioningID = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-partitioning/%06d", partitionings)}
			partitionings++
			item.partitioningOperation.Id = item.partitioningID.Name
			facts := item.partitioningOperation.OperationSemantics
			partitioningSlot := proto.CloneOf(subject)
			partitioningSlot.Kind = "ydb.partitioning"
			contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: item.partitioningID,
				Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.partitioningOperation},
				Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
					Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ},
						{Subject: partitioningSlot, Action: protocol.PlanningEffect_ALTER}}}})
			ordered = append(ordered, item.partitioningID)
		}
		for _, index := range item.indexes {
			prefix, number, action := "row-table-drop-index", &indexRemovals, protocol.PlanningEffect_DROP
			if index.change.add != nil {
				prefix, number, action = "row-table-add-index", &indexAdditions, protocol.PlanningEffect_CREATE
			}
			index.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", prefix, *number)}
			*number++
			index.operation.Id = index.id.Name
			facts := index.operation.OperationSemantics
			contribution.Steps = append(contribution.Steps, &protocol.PlanningStep{Id: index.id,
				Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: index.operation},
				Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
					Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ},
						{Subject: indexSubject(subject, index.change.name()), Action: action}}}})
			contribution.Dependencies = append(contribution.Dependencies, indexStepDependencies(*item, index, request.CommonSteps)...)
			ordered = append(ordered, index.id)
		}
		receiptSteps[item.index] = append(ordered, dropped...)
	}
	slices.SortFunc(columnTTLs, func(a, b columnTTLStep) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	for number, item := range columnTTLs {
		steps, dependencies, err := item.plan(number)
		if err != nil {
			return refused(item.index, err.Error()), nil
		}
		contribution.Steps = append(contribution.Steps, steps...)
		contribution.Dependencies = append(contribution.Dependencies, dependencies...)
		for _, step := range steps {
			receiptSteps[item.index] = append(receiptSteps[item.index], step.Id)
		}
	}
	// This service can replace only a complete common table creation. A column
	// step or a mixed footprint needs its own lifecycle, never a partial rewrite.
	claimed := make(map[int]bool)
	for _, step := range request.CommonSteps {
		if step.Parent == nil && step.AddedColumn == nil && len(step.GetMetadata().GetEffects()) == 0 {
			// An ordering marker such as common/prerequisite-creations belongs to no
			// table and changes nothing. The host keeps it.
			continue
		}
		if slices.ContainsFunc(alterations, func(item alteration) bool { return provider.CompareIdentities(step.Parent, item.table.Subject) == 0 }) {
			// The host keeps the column and setting steps of a table this service only alters.
			continue
		}
		if rebuilt := slices.IndexFunc(rebuilds, func(item *rebuild) bool { return provider.CompareIdentities(step.Parent, item.table.Subject) == 0 }); rebuilt >= 0 {
			// The new table takes every change the host would make to the old
			// one: it is written from the declaration.
			rebuilds[rebuilt].sources = append(rebuilds[rebuilt].sources, proto.CloneOf(step.Id))
			continue
		}
		if dropped := slices.IndexFunc(drops, func(item removal) bool { return provider.CompareIdentities(step.Parent, item.table.Subject) == 0 }); dropped >= 0 {
			// The table's DROP TABLE takes everything the host would drop of
			// it: the table, and the indexes and changefeeds it holds.
			drops[dropped].sources = append(drops[dropped].sources, proto.CloneOf(step.Id))
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
		if claimed[matched] || step.AddedColumn != nil || len(effects) == 0 ||
			effects[0].Action != protocol.PlanningEffect_CREATE || provider.CompareIdentities(effects[0].Subject, item.table.Subject) != 0 ||
			slices.ContainsFunc(effects[1:], func(effect *protocol.PlanningEffect) bool { return !createsIndexOf(effect, item.table.Subject) }) {
			return refused(item.index, "A table creation rewrite requires one complete common table footprint"), nil
		}
		claimed[matched] = true
		result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: []*protocol.StepIdentity{proto.CloneOf(step.Id)}, Replacement: proto.CloneOf(item.id)})
	}
	for _, item := range rebuilds {
		if len(item.sources) != 0 {
			result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: item.sources, Replacement: proto.CloneOf(item.steps[3].Id)})
		}
	}
	for _, item := range drops {
		if len(item.sources) != 0 {
			result.Rewrites = append(result.Rewrites, &protocol.PlanningRewrite{Sources: item.sources, Replacement: proto.CloneOf(item.id)})
		}
	}
	for i, change := range request.Changes {
		index := changeIndex(request, change)
		if index < 0 {
			return refusedChange(i, "A table property change needs the captured alteration of its table"), nil
		}
		result.Changes = append(result.Changes, changeReceipt(change, receiptSteps[index]))
	}
	for i, table := range request.Tables {
		if table.Action == protocol.PlanningTable_CONTEXT {
			continue
		}
		strategy := "Create the complete captured row table through the native table service"
		if slices.Contains(columnAlterations, i) {
			strategy = "The column table already holds the declared storage: its kind, its hash key and its shard count are fixed when YDB creates it, and its TTL is the declared one"
			if len(receiptSteps[i]) != 0 {
				strategy = "Reset the column table's TTL and set the declared one with YQL's ALTER TABLE through the query service; its kind, its hash key and its shard count are fixed when YDB creates it and already match"
			}
		}
		if table.Action == protocol.PlanningTable_DROP {
			strategy = "Drop the table, with its rows, indexes and changefeeds, through YQL's DROP TABLE, once the table still holds the columns and the key the plan read"
		}
		if slices.ContainsFunc(rebuilds, func(item *rebuild) bool { return item.index == i }) {
			strategy = "Rebuild the table, which YDB cannot change in place: create it anew from the declaration under a scratch name, copy the rows, swap the two tables by renaming them, and drop the old one"
		}
		if slices.ContainsFunc(creations, func(item creation) bool { return item.index == i && isColumnCreation(item.operation) }) {
			strategy = "Create the complete captured column table with YQL's CREATE TABLE through the query service, as the host's embedded engine writes it"
		}
		var steps []*protocol.StepIdentity
		for _, id := range receiptSteps[i] {
			steps = append(steps, proto.CloneOf(id))
		}
		if table.Action == protocol.PlanningTable_ALTER && !slices.Contains(columnAlterations, i) {
			strategy = "Add and drop columns, alter the column families and their column placements, set or drop the TTL, change the automatic partitioning settings, and add or drop global secondary indexes, through the native table service; changefeeds stay as they are, and other settings and column steps the native call cannot state stay with the host"
			if steps == nil {
				strategy = "The column families, column placements, TTL, partitioning settings and indexes already match the declaration, changefeeds stay as they are, and the host has no column step this service can carry out"
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

// createsIndexOf reports an effect that creates an index of table, which a
// creation builds with the table.
func createsIndexOf(effect *protocol.PlanningEffect, table *protocol.ComparisonIdentity) bool {
	subject := effect.GetSubject()
	if effect.GetAction() != protocol.PlanningEffect_CREATE || subject.GetKind() != "index" || subject.GetName().GetNormalized() == "" {
		return false
	}
	return provider.CompareIdentities(indexSubject(table, subject.GetName().GetSource()), subject) == 0
}

// ttlAction is what a TTL step does to the table's TTL setting: a removal drops
// it, a set on a table without one creates it, and any other set replaces it.
func ttlAction(change ttlChange) protocol.PlanningEffect_Action {
	switch {
	case change.drop:
		return protocol.PlanningEffect_DROP
	case change.held == nil:
		return protocol.PlanningEffect_CREATE
	default:
		return protocol.PlanningEffect_ALTER
	}
}
func cloneEffects(effects []*protocol.PlanningEffect) []*protocol.PlanningEffect {
	result := make([]*protocol.PlanningEffect, len(effects))
	for i, effect := range effects {
		result[i] = proto.CloneOf(effect)
	}
	return result
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
