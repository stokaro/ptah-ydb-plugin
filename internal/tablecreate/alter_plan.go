package tablecreate

import (
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// alteration is one planned change of an existing table: its column families,
// its TTL, its automatic partitioning settings with its read replicas and key
// bloom filter, its secondary indexes, and the host column steps this service
// replaces.
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
	indexSettings         []*indexSettingsStep
	feeds                 *changefeedStep
	claims                []*columnClaim
	defaults              []defaultStep
	inventory             []inventoryStep
	sequences             []sequenceStep
}

// indexStep is the native operation of one index change.
type indexStep struct {
	change    indexChange
	operation *protocol.NormalizedValue
	id        *protocol.StepIdentity
}

// indexSettingsStep is the operation of one change of an index's settings.
type indexSettingsStep struct {
	change    indexSettingsChange
	operation *protocol.NormalizedValue
	id        *protocol.StepIdentity
}

// alterationChanges are the changes planned for one altered table.
type alterationChanges struct {
	family        familyChange
	ttl           ttlChange
	partitioning  partitioningChange
	indexes       []indexChange
	indexSettings []indexSettingsChange
	changefeeds   []changefeeds.Change
}

func stepKey(id *protocol.StepIdentity) string { return id.GetOwner() + "\x00" + id.GetName() }

// planAlteration reads the families, column placements and TTL a captured ALTER
// carries on both sides and returns the changes that give the table what the
// declaration states. A non-empty reason refuses the whole request.
func planAlteration(owner *protocol.Reference, table *protocol.PlanningTable, steps []*protocol.CommonPlanningStep, capabilities map[string]bool) (alterationChanges, string, error) {
	desired, err := facetEnvelopes(owner, table.Desired.GetFeatures().GetFacets(), protocol.ValueInput_DESIRED)
	if err != nil {
		return alterationChanges{}, "", err
	}
	observed, err := facetEnvelopes(owner, table.Current.GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED)
	if err != nil {
		return alterationChanges{}, "", err
	}
	sides := &alterationSides{owner: owner, table: table, desired: desired, observed: observed, intent: &native.RowTableIntent{}, held: &native.DescribeTableResult{}}
	change, reason, err := sides.families(capabilities)
	if err != nil || reason != "" {
		return alterationChanges{}, reason, err
	}
	ttl, partitioning, reason := sides.settings()
	if reason != "" {
		return alterationChanges{}, reason, nil
	}
	indexes, reason, err := sides.indexes(steps, capabilities)
	if err != nil || reason != "" {
		return alterationChanges{}, reason, err
	}
	feeds, reason, err := sides.feeds(indexes, capabilities)
	if err != nil || reason != "" {
		return alterationChanges{}, reason, err
	}
	return alterationChanges{family: change, ttl: ttl, partitioning: partitioning, indexes: indexes.changes, indexSettings: indexes.settings, changefeeds: feeds}, "", nil
}

// alterationSides are the facets both sides of one altered table carry, with
// what planAlteration decodes from them on its way.
type alterationSides struct {
	owner    *protocol.Reference
	table    *protocol.PlanningTable
	desired  capturedFacets
	observed capturedFacets
	// intent is the declared table properties, empty where none are stated.
	intent *native.RowTableIntent
	// held is the table properties the table holds, empty where none were read.
	held *native.DescribeTableResult
	// family is what planFamilies reads.
	family familyInput
}

// families decodes the column families and the column placements of both
// sides and plans the family change.
func (a *alterationSides) families(capabilities map[string]bool) (familyChange, string, error) {
	in := &a.family
	*in = familyInput{placement: make(map[string]string), heldPlacement: make(map[string]string), existing: make(map[string]bool)}
	if reason, err := a.readDesiredFamilies(capabilities); err != nil || reason != "" {
		return familyChange{}, reason, err
	}
	if err := a.readHeldFamilies(); err != nil {
		return familyChange{}, "", err
	}
	stated := len(in.desired) != 0 || len(in.placement) != 0
	if stated && a.observed.table == nil {
		// Without the table's own read, a family the table already holds would look
		// missing, and adding it would fail after dispatch.
		return familyChange{}, "The table's current column families were not captured", nil
	}
	for name := range in.placement {
		if !slices.Contains(in.columns, name) {
			return familyChange{}, fmt.Sprintf("Column properties name column %q, which the table does not declare", name), nil
		}
	}
	// A source that describes where every column sits places a column it
	// names in no family in the default family, so a column the table holds
	// elsewhere moves back (see PlacedInDefault).
	for _, name := range in.columns {
		if _, placed := in.placement[name]; placed {
			continue
		}
		if held, read := in.heldPlacement[name]; read && familyKey(held) != DefaultFamily &&
			PlacedInDefault(a.table.Desired.GetFeatures().GetFacets().GetCoverage(), ColumnSubject(a.table.Subject, name)) {
			in.placement[name] = ""
		}
	}
	change, reason := planFamilies(*in)
	return change, reason, nil
}

// readDesiredFamilies decodes the declared table properties and column
// properties into the family input: the declared families, the family each
// column is placed in, the declared columns and key, and the columns the
// table holds. A cache mode the target cannot take is refused.
func (a *alterationSides) readDesiredFamilies(capabilities map[string]bool) (string, error) {
	in := &a.family
	if env := a.desired.table; env != nil {
		if err := protojson.Unmarshal(env.Payload, a.intent); err != nil {
			return "", err
		}
	}
	in.desired = a.intent.ColumnFamilies
	if reason := declaredCacheModeRefusal(tableLabel(a.owner), a.intent.ColumnFamilies, capabilities); reason != "" {
		return reason, nil
	}
	for name, env := range a.desired.columns {
		var column native.ColumnIntent
		if err := protojson.Unmarshal(env.Payload, &column); err != nil {
			return "", err
		}
		in.placement[name] = column.Family
	}
	for _, column := range a.table.Desired.GetColumns() {
		in.columns = append(in.columns, column.Name)
		if column.Primary {
			in.key = append(in.key, column.Name)
		}
	}
	in.key = append(in.key, a.table.Desired.GetPrimaryKey()...)
	in.key = append(in.key, a.intent.PrimaryKey...)
	for _, column := range a.table.Current.GetColumns() {
		in.existing[column.Name] = true
	}
	return "", nil
}

// readHeldFamilies decodes the held table properties and column properties
// into the family input: the families the table holds and the family each
// column sits in.
func (a *alterationSides) readHeldFamilies() error {
	if env := a.observed.table; env != nil {
		if err := protojson.Unmarshal(env.Payload, a.held); err != nil {
			return err
		}
	}
	a.family.held = a.held.ColumnFamilies
	for name, env := range a.observed.columns {
		var column native.ColumnMeta
		if err := protojson.Unmarshal(env.Payload, &column); err != nil {
			return err
		}
		a.family.heldPlacement[name] = column.Family
	}
	return nil
}

// settings plans the TTL change and the change of the automatic partitioning,
// the read replicas and the key bloom filter.
func (a *alterationSides) settings() (ttlChange, partitioningChange, string) {
	// A declaration that states table properties manages the TTL. So does a
	// source that describes the table properties of every table it declares
	// (complete desired knowledge): a table it states no properties for then
	// requests no TTL, and a TTL the table holds is removed. A source that
	// could not describe them leaves the TTL alone. Without the table's own
	// read, a TTL the table already holds would look absent, and a removal or a
	// set would be planned against the wrong state; a stated declaration is then
	// refused, and a table known only by its coverage is left alone.
	managed := a.desired.table != nil
	if managed && a.observed.table == nil {
		return ttlChange{}, partitioningChange{}, "The table's current TTL was not captured"
	}
	ttlManaged := managed || a.observed.table != nil &&
		FacetKnown(a.table.Desired.GetFeatures().GetFacets().GetCoverage(), schema.TableProperties, a.table.Subject, DesiredSide)
	ttl, reason := planTTL(ttlInput{managed: ttlManaged, desired: a.intent.TtlSettings, held: a.held.TtlSettings, declared: declaredTypes(a.table.Desired.GetColumns())})
	if reason != "" {
		return ttlChange{}, partitioningChange{}, reason
	}
	partitioning, reason := planPartitioning(partitioningInput{managed: managed, desired: a.intent.PartitioningSettings, held: a.held.PartitioningSettings, layout: layoutOf(a.intent),
		desiredBloom: a.intent.KeyBloomFilter, heldBloom: a.held.KeyBloomFilter, desiredReplicas: a.intent.ReadReplicasSettings, heldReplicas: a.held.ReadReplicasSettings})
	if reason != "" {
		return ttlChange{}, partitioningChange{}, reason
	}
	return ttl, partitioning, ""
}

// alterationIndexes are the index changes of an altered table, with the index
// sets of both sides they are planned from.
type alterationIndexes struct {
	wanted   *native.IndexSetIntent
	held     *native.IndexObservations
	changes  []indexChange
	settings []indexSettingsChange
}

// indexes plans the index changes of the table: its global secondary indexes
// and their settings, its vector and full-text indexes, and the host's common
// index steps this service carries out, which come first.
func (a *alterationSides) indexes(steps []*protocol.CommonPlanningStep, capabilities map[string]bool) (alterationIndexes, string, error) {
	// The index set is managed only by a declaration that states one. Without
	// the table's own read, an index the table already holds would look absent,
	// and adding it would fail after dispatch.
	result := alterationIndexes{wanted: &native.IndexSetIntent{}, held: &native.IndexObservations{}}
	if env := a.desired.indexes; env != nil {
		if err := protojson.Unmarshal(env.Payload, result.wanted); err != nil {
			return alterationIndexes{}, "", err
		}
	}
	if env := a.observed.indexes; env != nil {
		if err := protojson.Unmarshal(env.Payload, result.held); err != nil {
			return alterationIndexes{}, "", err
		}
	}
	if a.desired.indexes != nil && a.observed.indexes == nil {
		return alterationIndexes{}, "The table's current indexes were not captured", nil
	}
	indexes, indexSettings, reason := planIndexes(indexInput{managed: a.desired.indexes != nil, desired: result.wanted, held: result.held, columns: a.family.columns})
	if reason != "" {
		return alterationIndexes{}, reason, nil
	}
	// The vector and full-text indexes follow the rules of their own, with
	// the column types and the key they read.
	vectorAndText, reason := planNativeIndexes(nativeIndexInput{managed: a.desired.indexes != nil, desired: result.wanted, held: result.held,
		columns: declaredKeyTypes(a.table.Desired, a.intent), capabilities: capabilities})
	if reason != "" {
		return alterationIndexes{}, reason, nil
	}
	indexes = append(indexes, vectorAndText...)
	// The host adds and drops the common indexes it compares, and this
	// service carries out each step. An index both sides would change, a
	// vector or full-text index of the native set included, is declared twice.
	claimed, reason, err := claimIndexSteps(a.owner, a.table, steps, result.held, capabilities)
	if err != nil || reason != "" {
		return alterationIndexes{}, reason, err
	}
	for _, change := range claimed {
		if slices.ContainsFunc(indexes, func(other indexChange) bool { return other.name() == change.name() }) {
			return alterationIndexes{}, fmt.Sprintf("Index %q is declared both in the native index set and as a common index", change.name()), nil
		}
	}
	indexes = append(claimed, indexes...)
	if reason := uniqueAdditionRefusal(a.table.Subject, indexes, capabilities); reason != "" {
		return alterationIndexes{}, reason, nil
	}
	own, reason, err := a.ownIndexSettings(indexes, indexSettings, result.held)
	if err != nil || reason != "" {
		return alterationIndexes{}, reason, err
	}
	indexSettings = append(indexSettings, own...)
	result.changes, result.settings = indexes, indexSettings
	return result, "", nil
}

// ownIndexSettings plans the partitioning and read replicas an index's own
// facet declares, as a Go index directive or a YAML index entry writes them.
// An index whose settings the native index set changes as well is refused.
func (a *alterationSides) ownIndexSettings(indexes []indexChange, fromSet []indexSettingsChange, held *native.IndexObservations) ([]indexSettingsChange, string, error) {
	declaredSettings, _, err := indexSettingsFacets(a.owner, a.table.Desired.GetFeatures().GetFacets(), protocol.ValueInput_DESIRED)
	if err != nil {
		return nil, "", err
	}
	_, heldSettings, err := indexSettingsFacets(a.owner, a.table.Current.GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED)
	if err != nil {
		return nil, "", err
	}
	for _, change := range fromSet {
		if _, declared := declaredSettings[change.held.GetName()]; declared {
			return nil, fmt.Sprintf("Index %q declares its settings both in the native index set and as its own facet", change.held.GetName()), nil
		}
	}
	own, reason := planDeclaredIndexSettings(a.table, declaredSettings, heldSettings, indexes, held)
	return own, reason, nil
}

// feeds plans the changefeed changes of the table, against the indexes it
// holds and the ones its index changes drop and add.
func (a *alterationSides) feeds(indexes alterationIndexes, capabilities map[string]bool) ([]changefeeds.Change, string, error) {
	// The changefeeds are managed by a declaration that states them, and by a
	// source that describes the changefeeds of every table it declares. Without
	// the table's own read, a changefeed the table already holds would look
	// absent, and adding it would fail after dispatch.
	if a.desired.changefeeds != nil && a.observed.changefeeds == nil {
		return nil, "The table's current changefeeds were not captured", nil
	}
	indexNames := heldIndexNames(indexes.held)
	if a.desired.indexes != nil {
		indexNames = nil
		for _, index := range indexes.wanted.GetIndexes() {
			indexNames = append(indexNames, index.GetSpecification().GetName())
		}
	}
	// The table also holds the common indexes it declares, which the host
	// keeps out of the native set.
	for _, index := range a.table.Desired.GetIndexes() {
		if !slices.Contains(indexNames, index.GetName()) {
			indexNames = append(indexNames, index.GetName())
		}
	}
	// YDB never changes a table's key, so the declaration's first key column
	// is the one the table holds.
	keyType := firstKeyType(a.table.Desired)
	var droppedIndexes, addedIndexes []string
	for _, index := range indexes.changes {
		switch {
		case index.rename != nil:
			droppedIndexes = append(droppedIndexes, index.rename.held.Name)
			addedIndexes = append(addedIndexes, index.rename.to)
		case index.add != nil:
			addedIndexes = append(addedIndexes, index.name())
		default:
			droppedIndexes = append(droppedIndexes, index.name())
		}
	}
	return planChangefeeds(changefeedInput{owner: a.owner, desired: a.desired.changefeeds, held: a.observed.changefeeds, indexes: indexNames,
		droppedIndexes: droppedIndexes, addedIndexes: addedIndexes, keyType: keyType, capabilities: capabilities,
		managed: changefeedsManaged(a.table, a.desired, a.observed)})
}

// UniqueIndexOnExistingTableCapability is a unique index added to a table
// that exists, as the host's vocabulary spells it.
const UniqueIndexOnExistingTableCapability = "unique_index_on_existing_table"

// uniqueAdditionRefusal refuses, in the host's embedded engine's words, a
// unique index added to table, which exists, on a session without
// unique_index_on_existing_table. YDB refuses one there after the call
// (`Adding a unique index to an existing table is disabled`): 25.1 and 25.2
// always, and later lines with EnableAddUniqueIndex off, its default. A table
// the plan creates takes its unique indexes in its CreateTable request and
// needs no capability.
func uniqueAdditionRefusal(table *protocol.ComparisonIdentity, changes []indexChange, capabilities map[string]bool) string {
	if capabilities[UniqueIndexOnExistingTableCapability] {
		return ""
	}
	for _, change := range changes {
		if change.add.GetGlobalUniqueIndex() != nil {
			return fmt.Sprintf("adding unique index %q to table %q, which exists already, which requires target capability %s, unavailable on this ydb target%s",
				change.add.GetName(), table.GetSchema().GetSource()+"."+table.GetName().GetSource(), UniqueIndexOnExistingTableCapability,
				featureflags.Clause(UniqueIndexOnExistingTableCapability))
		}
	}
	return ""
}

// heldIndexNames lists the indexes a table holds.
func heldIndexNames(held *native.IndexObservations) []string {
	var names []string
	for _, index := range held.GetIndexes() {
		names = append(names, index.GetDescription().GetName())
	}
	return names
}

// capturedFacets are the table and column facets one side of a table carries.
type capturedFacets struct {
	table       *protocol.Envelope
	indexes     *protocol.Envelope
	changefeeds *protocol.Envelope
	columns     map[string]*protocol.Envelope
}

// facetEnvelopes returns the table and column properties a captured state
// carries for owner, with the index set and the changefeed set of the table. A
// facet excluded for this target, a facet bound to other targets and a kind
// this service does not own state nothing here.
func facetEnvelopes(owner *protocol.Reference, state *protocol.ComparisonState, representation protocol.ValueInput_Representation) (capturedFacets, error) {
	result := capturedFacets{columns: make(map[string]*protocol.Envelope)}
	if state == nil {
		return result, nil
	}
	if state.Representation != representation {
		return result, fmt.Errorf("YDB table alteration requires %s facets", representation)
	}
	for _, record := range state.Records {
		if record.Kind != schema.TableProperties && record.Kind != schema.ColumnProperties && record.Kind != schema.IndexProperties && record.Kind != schema.ChangefeedProperties {
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
		case schema.ChangefeedProperties:
			result.changefeeds = value
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
	moved := make(map[string]bool)
	for _, move := range item.change.move {
		moved[move.Name] = true
	}
	replacement := make(map[string]*protocol.StepIdentity)
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
	replacement := make(map[string]*protocol.StepIdentity)
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
	replacement := make(map[string]*protocol.StepIdentity)
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
