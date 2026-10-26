package tablecreate

import (
	"fmt"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ChangefeedDifference reports whether the table the observed changefeed set
// describes does not hold the declared one. A non-empty undecided says why the
// move cannot be decided: a changefeed the read cannot represent, one the
// declaration cannot state, or a replication-managed stream the declaration
// changes. The comparison reports such a table as undecided and the planner
// leaves its changefeeds as they are, so both read the move through
// changefeeds.Diff and cannot disagree.
func ChangefeedDifference(desired, observed *protocol.Envelope) (differs bool, undecided string, err error) {
	want, held, err := changefeedSets(desired, observed)
	if err != nil {
		return false, "", err
	}
	steps, undecided := changefeeds.Diff(want, held)
	return len(steps) > 0, undecided, nil
}

// UnstatedChangefeedFacet returns the desired changefeed set of a table whose
// source states none: the empty set, which a source that describes the
// changefeeds of every table it declares means.
func UnstatedChangefeedFacet(owner *protocol.Reference) (*protocol.Envelope, error) {
	payload, err := protojson.Marshal(&native.ChangefeedSetIntent{})
	if err != nil {
		return nil, err
	}
	model := schema.NativeDesiredModel(schema.ChangefeedProperties)
	return schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: proto.CloneOf(owner), Payload: payload})
}

// changefeedSets decodes both changefeed facets, nil desired standing for the
// empty set.
func changefeedSets(desired, observed *protocol.Envelope) (*native.ChangefeedSetIntent, *native.ChangefeedObservations, error) {
	want := &native.ChangefeedSetIntent{}
	if desired != nil {
		if err := protojson.Unmarshal(desired.Payload, want); err != nil {
			return nil, nil, err
		}
	}
	held := &native.ChangefeedObservations{}
	if observed != nil {
		if err := protojson.Unmarshal(observed.Payload, held); err != nil {
			return nil, nil, err
		}
	}
	return want, held, nil
}

// changefeedStep is the native operation that carries every changefeed change
// of one table. The host runs a step that drops an object after every step
// that creates one, so the changes of one table are one step: the operation
// drops, then adds, then changes topics, which a swap at YDB's limit on the
// changefeeds of one table and a changefeed dropped and added again need.
type changefeedStep struct {
	changes   []changefeeds.Change
	operation *protocol.NormalizedValue
	id        *protocol.StepIdentity
}

// changefeedInput is what the changefeed rule reads for one table.
type changefeedInput struct {
	owner *protocol.Reference
	// managed is true when the declaration states the table's changefeeds:
	// it carries the facet, or its source describes the changefeeds of every
	// table it declares and this one has none.
	managed bool
	// desired and held are the two facets, desired nil for the empty set.
	desired, held *protocol.Envelope
	// indexes are the names of the indexes the table is to have, which share
	// the table's path with its changefeeds.
	indexes []string
	// droppedIndexes and addedIndexes are the indexes the plan drops and adds.
	droppedIndexes, addedIndexes []string
	// keyType is the YDB type of the table's first key column, empty where
	// unknown.
	keyType      string
	capabilities map[string]bool
}

// planChangefeeds returns the changes that give the table the declared
// changefeeds, or the reason the declaration cannot be written on this
// target. A move the comparison leaves undecided plans nothing: the
// changefeeds stay as they are.
func planChangefeeds(in changefeedInput) ([]changefeeds.Change, string, error) {
	if !in.managed {
		return nil, "", nil
	}
	want, held, err := changefeedSets(in.desired, in.held)
	if err != nil {
		return nil, "", err
	}
	changes, undecided := changefeeds.Diff(want, held)
	if undecided != "" {
		return nil, "", nil
	}
	label := tableLabel(in.owner)
	declared, err := changefeeds.Declared(want)
	if err != nil {
		return nil, "", err
	}
	if reason := changefeeds.NameRefusal(declared, in.indexes); reason != "" {
		return nil, fmt.Sprintf("table %q: %s", label, reason), nil
	}
	for _, change := range changes {
		action := change.Action()
		// The host adds an index before it drops an object and drops an
		// object after every addition, so a name an index frees in the same
		// plan, or an index takes from a dropped changefeed, cannot be ordered.
		if (action == changefeeds.Add || action == changefeeds.Recreate) && slices.Contains(in.droppedIndexes, change.Name) {
			return nil, fmt.Sprintf("%s takes the name of an index the plan drops, and the host adds it before the index is gone; drop the index in one apply and add the changefeed in the next",
				changefeeds.Subject(label, change.Name)), nil
		}
		if (action == changefeeds.Drop || action == changefeeds.Recreate) && slices.Contains(in.addedIndexes, change.Name) {
			return nil, fmt.Sprintf("%s has the name of an index the plan adds, and the host adds the index before the changefeed is gone; drop the changefeed in one apply and add the index in the next",
				changefeeds.Subject(label, change.Name)), nil
		}
		if change.After == nil {
			continue
		}
		feed, err := changefeeds.FromIntent(change.After)
		if err != nil {
			return nil, "", err
		}
		if refusal := changefeeds.Check(label, feed, in.capabilities); refusal != nil {
			return nil, refusal.Message(), nil
		}
		if action == changefeeds.Add || action == changefeeds.Recreate {
			if reason := changefeeds.KeyRefusal(feed, in.keyType); reason != "" {
				return nil, changefeeds.Subject(label, feed.Name) + ": " + reason, nil
			}
		}
	}
	return changes, "", nil
}

// captureChangefeedStep lowers a table's changefeed changes to one native
// operation, nil for none, or returns the reason it cannot be captured.
func captureChangefeedStep(owner *protocol.Reference, changes []changefeeds.Change) (*changefeedStep, string) {
	if len(changes) == 0 {
		return nil, ""
	}
	operation, err := schema.CaptureChangefeedOperation("pending", owner, changes)
	if err != nil {
		return nil, err.Error()
	}
	return &changefeedStep{changes: changes, operation: operation}, ""
}

// planningStep names a table's changefeed step and returns it with the edges
// that order it after the given steps of the same table, such as its creation.
// Its effects name each changefeed it creates, drops, or changes; a changefeed
// it drops and adds again is changed.
func (item *changefeedStep) planningStep(number int, subject *protocol.ComparisonIdentity, after []*protocol.StepIdentity) (*protocol.PlanningStep, []*protocol.PlanningDependency) {
	item.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-changefeeds/%06d", number)}
	item.operation.Id = item.id.Name
	facts := item.operation.OperationSemantics
	effects := []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ}}
	for _, change := range item.changes {
		action := protocol.PlanningEffect_ALTER
		switch change.Action() {
		case changefeeds.Add:
			action = protocol.PlanningEffect_CREATE
		case changefeeds.Drop:
			action = protocol.PlanningEffect_DROP
		}
		effects = append(effects, &protocol.PlanningEffect{Subject: changefeedSubject(subject, change.Name), Action: action})
	}
	step := &protocol.PlanningStep{Id: item.id, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.operation},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: effects}}
	var dependencies []*protocol.PlanningDependency
	for _, before := range after {
		dependencies = append(dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(before), After: proto.CloneOf(item.id)})
	}
	return step, dependencies
}

// changefeedSubject identifies one changefeed of the table, so that two
// changefeeds of one table never share an effect subject.
func changefeedSubject(table *protocol.ComparisonIdentity, name string) *protocol.ComparisonIdentity {
	subject := proto.CloneOf(table)
	subject.Kind, subject.Parent = "ydb.changefeed", proto.CloneOf(table.GetName())
	subject.Name = &protocol.ComparisonPart{Source: name, Normalized: name}
	return subject
}

// plannedCreationChangefeeds returns the step that adds the changefeeds a new
// table declares, nil for none, or the reason they cannot be written on this target. A
// column table takes no changefeed.
func plannedCreationChangefeeds(owner *protocol.Reference, table *protocol.DeclaredTable, capabilities map[string]bool) (*changefeedStep, string, error) {
	facets, err := facetEnvelopes(owner, table.GetFeatures().GetFacets(), protocol.ValueInput_DESIRED)
	if err != nil {
		return nil, "", err
	}
	if facets.changefeeds == nil {
		return nil, "", nil
	}
	if IsColumnTable(table) {
		return nil, fmt.Sprintf("table %q: %s", tableLabel(owner), schema.ColumnTableRowSettingsRefusal), nil
	}
	var indexes []string
	if facets.indexes != nil {
		var declared native.IndexSetIntent
		if err := protojson.Unmarshal(facets.indexes.Payload, &declared); err != nil {
			return nil, "", err
		}
		for _, index := range declared.GetIndexes() {
			indexes = append(indexes, index.GetSpecification().GetName())
		}
	}
	for _, index := range table.GetIndexes() {
		indexes = append(indexes, index.GetName())
	}
	changes, reason, err := planChangefeeds(changefeedInput{owner: owner, managed: true, desired: facets.changefeeds, indexes: indexes,
		keyType: firstKeyType(table), capabilities: capabilities})
	if err != nil || reason != "" {
		return nil, reason, err
	}
	feeds, reason := captureChangefeedStep(owner, changes)
	return feeds, reason, nil
}

// firstKeyType is the YDB type the first key column of a declaration maps to,
// or empty where the declaration states no key column this provider maps.
func firstKeyType(table *protocol.DeclaredTable) string {
	key := ""
	if keys := table.GetPrimaryKey(); len(keys) > 0 {
		key = keys[0]
	}
	for _, column := range table.GetColumns() {
		if key == "" && column.GetPrimary() {
			key = column.GetName()
		}
	}
	for _, column := range table.GetColumns() {
		if column.GetName() != key {
			continue
		}
		value, err := schema.StorageType(column.GetType(), column.GetNullable())
		if err != nil {
			return ""
		}
		name, _, err := schema.ColumnType(value)
		if err != nil {
			return ""
		}
		return name
	}
	return ""
}
