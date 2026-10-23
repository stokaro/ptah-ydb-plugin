package tablecreate

import (
	"fmt"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// alteration is one planned change of an existing table: its column families,
// its TTL, its automatic partitioning settings, its secondary indexes, and the
// host column steps this service replaces.
type alteration struct {
	index                 int
	table                 *protocol.PlanningTable
	change                familyChange
	operation             *protocol.NormalizedValue
	id                    *protocol.StepIdentity
	ttl                   ttlChange
	ttlOperation          *protocol.NormalizedValue
	ttlID                 *protocol.StepIdentity
	partitioning          partitioningChange
	partitioningOperation *protocol.NormalizedValue
	partitioningID        *protocol.StepIdentity
	indexes               []*indexStep
	claims                []*columnClaim
}

// indexStep is the native operation of one index change.
type indexStep struct {
	change    indexChange
	operation *protocol.NormalizedValue
	id        *protocol.StepIdentity
}

// alterationChanges are the changes planned for one altered table.
type alterationChanges struct {
	family       familyChange
	ttl          ttlChange
	partitioning partitioningChange
	indexes      []indexChange
}

func stepKey(id *protocol.StepIdentity) string { return id.GetOwner() + "\x00" + id.GetName() }

// planAlteration reads the families, column placements and TTL a captured ALTER
// carries on both sides and returns the changes that give the table what the
// declaration states. A non-empty reason refuses the whole request.
func planAlteration(owner *protocol.Reference, table *protocol.PlanningTable) (alterationChanges, string, error) {
	desired, err := facetEnvelopes(owner, table.Desired.GetFeatures().GetFacets(), protocol.ValueInput_DESIRED)
	if err != nil {
		return alterationChanges{}, "", err
	}
	observed, err := facetEnvelopes(owner, table.Current.GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED)
	if err != nil {
		return alterationChanges{}, "", err
	}
	in := familyInput{placement: map[string]string{}, heldPlacement: map[string]string{}, existing: map[string]bool{}}
	var intent native.RowTableIntent
	if env := desired.table; env != nil {
		if err := protojson.Unmarshal(env.Payload, &intent); err != nil {
			return alterationChanges{}, "", err
		}
	}
	in.desired = intent.ColumnFamilies
	for name, env := range desired.columns {
		var column native.ColumnIntent
		if err := protojson.Unmarshal(env.Payload, &column); err != nil {
			return alterationChanges{}, "", err
		}
		in.placement[name] = column.Family
	}
	for _, column := range table.Desired.GetColumns() {
		in.columns = append(in.columns, column.Name)
		if column.Primary {
			in.key = append(in.key, column.Name)
		}
	}
	in.key = append(in.key, table.Desired.GetPrimaryKey()...)
	in.key = append(in.key, intent.PrimaryKey...)
	for _, column := range table.Current.GetColumns() {
		in.existing[column.Name] = true
	}
	var held native.DescribeTableResult
	if env := observed.table; env != nil {
		if err := protojson.Unmarshal(env.Payload, &held); err != nil {
			return alterationChanges{}, "", err
		}
	}
	in.held = held.ColumnFamilies
	for name, env := range observed.columns {
		var column native.ColumnMeta
		if err := protojson.Unmarshal(env.Payload, &column); err != nil {
			return alterationChanges{}, "", err
		}
		in.heldPlacement[name] = column.Family
	}
	stated := len(in.desired) != 0 || len(in.placement) != 0
	if stated && observed.table == nil {
		// Without the table's own read, a family the table already holds would look
		// missing, and adding it would fail after dispatch.
		return alterationChanges{}, "The table's current column families were not captured", nil
	}
	for name := range in.placement {
		if !slices.Contains(in.columns, name) {
			return alterationChanges{}, fmt.Sprintf("Column properties name column %q, which the table does not declare", name), nil
		}
	}
	change, reason := planFamilies(in)
	if reason != "" {
		return alterationChanges{}, reason, nil
	}
	// The TTL is managed only by a declaration that states table properties.
	// Without the table's own read, a TTL the table already holds would look
	// absent, and a removal or a set would be planned against the wrong state.
	managed := desired.table != nil
	if managed && observed.table == nil {
		return alterationChanges{}, "The table's current TTL was not captured", nil
	}
	ttl, reason := planTTL(ttlInput{managed: managed, desired: intent.TtlSettings, held: held.TtlSettings, declared: declaredTypes(table.Desired.GetColumns())})
	if reason != "" {
		return alterationChanges{}, reason, nil
	}
	partitioning, reason := planPartitioning(partitioningInput{managed: managed, desired: intent.PartitioningSettings, held: held.PartitioningSettings})
	if reason != "" {
		return alterationChanges{}, reason, nil
	}
	// The index set is managed only by a declaration that states one. Without
	// the table's own read, an index the table already holds would look absent,
	// and adding it would fail after dispatch.
	var wantedIndexes native.IndexSetIntent
	if env := desired.indexes; env != nil {
		if err := protojson.Unmarshal(env.Payload, &wantedIndexes); err != nil {
			return alterationChanges{}, "", err
		}
	}
	var heldIndexes native.IndexObservations
	if env := observed.indexes; env != nil {
		if err := protojson.Unmarshal(env.Payload, &heldIndexes); err != nil {
			return alterationChanges{}, "", err
		}
	}
	if desired.indexes != nil && observed.indexes == nil {
		return alterationChanges{}, "The table's current indexes were not captured", nil
	}
	indexes, reason := planIndexes(indexInput{managed: desired.indexes != nil, desired: &wantedIndexes, held: &heldIndexes, columns: in.columns})
	if reason != "" {
		return alterationChanges{}, reason, nil
	}
	return alterationChanges{family: change, ttl: ttl, partitioning: partitioning, indexes: indexes}, "", nil
}

// capturedFacets are the table and column facets one side of a table carries.
type capturedFacets struct {
	table   *protocol.Envelope
	indexes *protocol.Envelope
	columns map[string]*protocol.Envelope
}

// facetEnvelopes returns the table and column properties a captured state
// carries for owner, with the index set of the table. A facet excluded for this target, a facet bound to other
// targets and a kind this service does not own state nothing here.
func facetEnvelopes(owner *protocol.Reference, state *protocol.ComparisonState, representation protocol.ValueInput_Representation) (capturedFacets, error) {
	result := capturedFacets{columns: map[string]*protocol.Envelope{}}
	if state == nil {
		return result, nil
	}
	if state.Representation != representation {
		return result, fmt.Errorf("YDB table alteration requires %s facets", representation)
	}
	for _, record := range state.Records {
		if record.Kind != schema.TableProperties && record.Kind != schema.ColumnProperties && record.Kind != schema.IndexProperties {
			continue
		}
		if (len(record.Targets) > 0 && !slices.Contains(record.Targets, "ydb")) || record.Value == nil {
			continue
		}
		value, err := normalizedFacet(record, representation)
		if err != nil {
			return result, err
		}
		identity := proto.CloneOf(owner)
		if value.Kind == schema.ColumnProperties {
			identity.Kind, identity.Path, identity.Name = "column", []string{owner.Name}, value.Owner.Name
		}
		if !proto.Equal(identity, value.Owner) {
			return result, fmt.Errorf("YDB alteration facet belongs to a different owner")
		}
		// The SDK refuses two facets of one kind and subject before this runs.
		switch value.Kind {
		case schema.TableProperties:
			result.table = value
			continue
		case schema.IndexProperties:
			result.indexes = value
			continue
		}
		result.columns[value.Owner.Name] = value
	}
	return result, nil
}

// alterStepDependencies orders the family operation against the column steps of
// the same table, whoever carries them out. The families change after every
// column addition it moves a column into, since a column moves only once it
// exists, and before every column removal, so a removal never meets a family the
// plan is still reshaping. A column the plan removes is never named by the
// alteration. A claimed step is named by its native replacement, so the edge
// joins two steps of this service; any other step keeps its host identity.
func alterStepDependencies(item alteration, steps []*protocol.CommonPlanningStep) []*protocol.PlanningDependency {
	moved := map[string]bool{}
	for _, move := range item.change.move {
		moved[move.Name] = true
	}
	replacement := map[string]*protocol.StepIdentity{}
	for _, claim := range item.claims {
		replacement[stepKey(claim.source.Id)] = claim.id
	}
	identity := func(step *protocol.CommonPlanningStep) *protocol.StepIdentity {
		if claimed := replacement[stepKey(step.Id)]; claimed != nil {
			return proto.CloneOf(claimed)
		}
		return proto.CloneOf(step.Id)
	}
	var dependencies []*protocol.PlanningDependency
	for _, step := range steps {
		if step.Parent == nil || provider.CompareIdentities(step.Parent, item.table.Subject) != 0 {
			continue
		}
		switch {
		case step.AddedColumn != nil && moved[step.AddedColumn.Name]:
			dependencies = append(dependencies, &protocol.PlanningDependency{Before: identity(step), After: proto.CloneOf(item.id)})
		case step.AddedColumn == nil && dropsColumn(step):
			dependencies = append(dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(item.id), After: identity(step)})
		}
	}
	return dependencies
}

// dropsColumn reports whether a host step removes a column.
func dropsColumn(step *protocol.CommonPlanningStep) bool {
	return slices.ContainsFunc(step.GetMetadata().GetEffects(), func(effect *protocol.PlanningEffect) bool {
		return effect.Action == protocol.PlanningEffect_DROP && effect.GetSubject().GetKind() == "column"
	})
}

// ttlStepDependencies orders the TTL operation against the column steps of the
// same table, whoever carries them out. A TTL set follows the addition of the
// column it reads, since the server refuses a TTL on a column the table does
// not hold. A TTL change or removal precedes the removal of the column the
// table's TTL reads now, since the server refuses to drop that column while the
// TTL reads it. A claimed step is named by its native replacement, as in
// alterStepDependencies.
func ttlStepDependencies(item alteration, steps []*protocol.CommonPlanningStep) []*protocol.PlanningDependency {
	replacement := map[string]*protocol.StepIdentity{}
	for _, claim := range item.claims {
		replacement[stepKey(claim.source.Id)] = claim.id
	}
	identity := func(step *protocol.CommonPlanningStep) *protocol.StepIdentity {
		if claimed := replacement[stepKey(step.Id)]; claimed != nil {
			return proto.CloneOf(claimed)
		}
		return proto.CloneOf(step.Id)
	}
	reads := ttlColumn(item.ttl.set)
	held := ttlColumn(item.ttl.held)
	var dependencies []*protocol.PlanningDependency
	for _, step := range steps {
		if step.Parent == nil || provider.CompareIdentities(step.Parent, item.table.Subject) != 0 {
			continue
		}
		switch {
		case step.AddedColumn != nil && reads != "" && step.AddedColumn.Name == reads:
			dependencies = append(dependencies, &protocol.PlanningDependency{Before: identity(step), After: proto.CloneOf(item.ttlID)})
		case step.AddedColumn == nil && held != "" && dropsColumn(step) && dropsNamed(step, item.table.Subject, held):
			dependencies = append(dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(item.ttlID), After: identity(step)})
		}
	}
	return dependencies
}

// dropsNamed reports whether a host step that removes a column may remove the
// named one. A step whose footprint does not name its column is assumed to.
func dropsNamed(step *protocol.CommonPlanningStep, table *protocol.ComparisonIdentity, name string) bool {
	column, _, shaped := columnFootprint(step, table)
	return !shaped || column == name
}

// indexSubject identifies one index of the table the way the host does, so
// that two index steps of one table never share an effect subject.
func indexSubject(table *protocol.ComparisonIdentity, name string) *protocol.ComparisonIdentity {
	subject := proto.CloneOf(table)
	subject.Kind, subject.Parent = "index", proto.CloneOf(table.GetName())
	subject.Name = &protocol.ComparisonPart{Source: name, Normalized: name}
	return subject
}

// indexStepDependencies orders an index step against the column steps of the
// same table, whoever carries them out. An index is added after the addition of
// every column it reads, since the server refuses an index on a column the
// table does not hold. An index is dropped before the removal of every column
// it reads, since the server refuses to drop a column an index reads. A column
// step that does not name its column is assumed to remove any column. A claimed
// step is named by its native replacement, as in alterStepDependencies.
func indexStepDependencies(item alteration, index *indexStep, steps []*protocol.CommonPlanningStep) []*protocol.PlanningDependency {
	replacement := map[string]*protocol.StepIdentity{}
	for _, claim := range item.claims {
		replacement[stepKey(claim.source.Id)] = claim.id
	}
	identity := func(step *protocol.CommonPlanningStep) *protocol.StepIdentity {
		if claimed := replacement[stepKey(step.Id)]; claimed != nil {
			return proto.CloneOf(claimed)
		}
		return proto.CloneOf(step.Id)
	}
	reads := index.change.columns()
	var dependencies []*protocol.PlanningDependency
	for _, step := range steps {
		if step.Parent == nil || provider.CompareIdentities(step.Parent, item.table.Subject) != 0 {
			continue
		}
		switch {
		case index.change.add != nil && step.AddedColumn != nil && slices.Contains(reads, step.AddedColumn.Name):
			dependencies = append(dependencies, &protocol.PlanningDependency{Before: identity(step), After: proto.CloneOf(index.id)})
		case index.change.drop != nil && step.AddedColumn == nil && dropsColumn(step) && dropsAny(step, item.table.Subject, reads):
			dependencies = append(dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(index.id), After: identity(step)})
		}
	}
	return dependencies
}

// dropsAny reports whether a host step that removes a column may remove one of
// the named ones. A step whose footprint does not name its column is assumed to.
func dropsAny(step *protocol.CommonPlanningStep, table *protocol.ComparisonIdentity, names []string) bool {
	column, _, shaped := columnFootprint(step, table)
	return !shaped || slices.Contains(names, column)
}
