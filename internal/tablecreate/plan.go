package tablecreate

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ServiceID names the row-table lifecycle assessment.
const ServiceID = "ydb-row-table-create"

// ParentKinds assigns the native table and column settings, the storage of a
// column table, and the changefeeds of a table to this assessment: the host
// plans no table whose facets no planning service claims.
func ParentKinds() []string {
	return []string{schema.TableProperties, schema.ColumnProperties, schema.IndexProperties, schema.IndexSettingsProperties, schema.ChangefeedProperties, schema.ColumnStorageProperties}
}

type creation struct {
	index     int
	table     *protocol.PlanningTable
	operation *protocol.NormalizedValue
	id        *protocol.StepIdentity
	// feeds adds the changefeeds the new table declares, after its creation.
	feeds *changefeedStep
	// indexes add the vector and full-text indexes the new table declares,
	// each after its creation.
	indexes []*indexStep
	// settings give the new table's global indexes the partitioning and read
	// replicas their own facets declare, each after the creation: the call
	// that creates a table takes no index settings.
	settings []*indexSettingsStep
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
	p := &planner{
		request:      request,
		result:       &protocol.PlannedFeatures{Complete: true},
		names:        requestTableNames(request),
		contribution: &protocol.PlanningContribution{Owner: schema.ProviderID},
		receiptSteps: make(map[int][]*protocol.StepIdentity),
	}
	for i, table := range request.Tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if refusal, err := p.planTable(i, table); err != nil || refusal != nil {
			return refusal, err
		}
	}
	if refusal, err := p.contributeCreations(); err != nil || refusal != nil {
		return refusal, err
	}
	if err := p.contributeDrops(); err != nil {
		return nil, err
	}
	p.contributeRebuilds()
	p.contributeAlterations()
	if refusal := p.contributeColumnTables(); refusal != nil {
		return refusal, nil
	}
	if refusal, err := p.rewriteCommonSteps(); err != nil || refusal != nil {
		return refusal, err
	}
	if refusal := p.rewriteReplacedTables(); refusal != nil {
		return refusal, nil
	}
	if refusal := p.receiveChanges(); refusal != nil {
		return refusal, nil
	}
	p.receiveTables()
	p.contribution.Dependencies = append(p.contribution.Dependencies, commonObjectDependencies(request.CommonSteps, p.contribution.Steps)...)
	if len(p.contribution.Steps) > 0 {
		p.result.Contributions = []*protocol.PlanningContribution{p.contribution}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.result, nil
}

// planner is the state Plan builds across its phases: the tables the request
// creates, drops, rebuilds and alters, the contribution that takes their
// steps, and the steps each table's receipt names. Each phase that can refuse
// returns the refusal that answers the whole request, or nil to go on.
type planner struct {
	request *protocol.PlanFeaturesRequest
	result  *protocol.PlannedFeatures
	// names are the tables the request declares or holds, which a rebuild's
	// scratch name must not take.
	names       map[string]bool
	creations   []creation
	drops       []removal
	rebuilds    []*rebuild
	alterations []alteration
	// columnAlterations are the column tables the request alters. Each holds
	// what the declaration asks for, or differs from it in its local indexes
	// and its TTL alone, since every other change is refused. columnChanges
	// are those changes.
	columnAlterations []int
	columnChanges     []columnTableStep
	contribution      *protocol.PlanningContribution
	// receiptSteps are the steps of each table, by its index in the request.
	receiptSteps map[int][]*protocol.StepIdentity
	// feedNumber numbers the changefeed steps of the created and the altered
	// tables.
	feedNumber int
	// indexSettingsNumber numbers the index settings steps of the created and
	// the altered tables.
	indexSettingsNumber int
}

// planTable plans table i of the request by its action. A context table is
// the host's, and nothing is planned for it.
func (p *planner) planTable(i int, table *protocol.PlanningTable) (*protocol.PlannedFeatures, error) {
	request := p.request
	if table.Action == protocol.PlanningTable_CONTEXT {
		return nil, nil
	}
	if !slices.Equal(request.ParentKinds, ParentKinds()) {
		return nil, fmt.Errorf("invalid YDB table parent assignment")
	}
	if table.Action == protocol.PlanningTable_REBUILD {
		return p.planTableRebuild(i, table)
	}
	if table.Action == protocol.PlanningTable_DROP {
		return p.planTableDrop(i, table)
	}
	if table.Action != protocol.PlanningTable_CREATE && table.Action != protocol.PlanningTable_ALTER {
		return refused(i, "Only native row-table creation, alteration of column families, columns, the TTL, the table settings and the indexes, column-table creation and table removal are implemented"), nil
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
		return p.planTableAlteration(i, table, owner)
	}
	return p.planTableCreation(i, table, owner)
}

// planTableRebuild plans the REBUILD the host proposes for table i, which the
// policy has to allow and a column table never takes.
func (p *planner) planTableRebuild(i int, table *protocol.PlanningTable) (*protocol.PlannedFeatures, error) {
	request := p.request
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
	desired, err := Compile(owner, table.Desired, request.Capabilities)
	if err != nil {
		return refused(i, fmt.Sprintf("table %q: %v", tableLabel(owner), err)), nil
	}
	return p.addRebuild(i, owner, table, desired)
}

// addRebuild plans the rebuild of table i into the request desired, and
// keeps it with the rebuilds of the plan.
func (p *planner) addRebuild(i int, owner *protocol.Reference, table *protocol.PlanningTable, desired *Ydb_Table.CreateTableRequest) (*protocol.PlannedFeatures, error) {
	item, reason, err := planRebuild(len(p.rebuilds), owner, table, desired, p.names, p.request.Capabilities, p.request.CommonSteps, p.request.Changes)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return refused(i, reason), nil
	}
	item.index = i
	p.rebuilds = append(p.rebuilds, item)
	return nil, nil
}

// planTableDrop captures the removal of table i, which needs nontransactional
// DDL and the read of the table it drops.
func (p *planner) planTableDrop(i int, table *protocol.PlanningTable) (*protocol.PlannedFeatures, error) {
	transactional, known := p.request.Capabilities["transactional_ddl"]
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
	p.drops = append(p.drops, removal{index: i, table: table, operation: operation})
	return nil, nil
}

// planTableAlteration plans the ALTER of table i: the change of a column
// table, the rebuild a row table's change needs, or the native alteration of
// a row table.
func (p *planner) planTableAlteration(i int, table *protocol.PlanningTable, owner *protocol.Reference) (*protocol.PlannedFeatures, error) {
	request := p.request
	columnPlan, err := planColumnAlteration(owner, table, request.CommonSteps, request.Capabilities)
	if err != nil {
		return nil, err
	}
	if columnPlan.reason != "" {
		return refused(i, columnPlan.reason), nil
	}
	if columnPlan.columnTable {
		if slices.Contains(request.TargetFeatures, provider.ColumnsFeature) {
			if reason := completeColumnAlteration(owner, table, nil, nil, nil, nil, request.Capabilities); reason != "" {
				return refused(i, reason), nil
			}
		}
		p.columnAlterations = append(p.columnAlterations, i)
		if columnPlan.change != nil {
			p.columnChanges = append(p.columnChanges, columnTableStep{index: i, table: table, owner: owner, root: cmp.Or(request.DatabasePath, owner.Database), change: *columnPlan.change})
		}
		return nil, nil
	}
	if needs, desired := rebuildNeeds(owner, table, request.Capabilities); len(needs) != 0 {
		if !request.GetPolicy().GetAllowTableRebuild() {
			return refused(i, rebuildRefusal(needs[0], request.GetPolicy())), nil
		}
		return p.addRebuild(i, owner, table, desired)
	}
	return p.planRowAlteration(i, table, owner)
}

// planRowAlteration plans the native alteration of row table i: the changes
// planAlteration finds, their operations, and the column steps of the table.
func (p *planner) planRowAlteration(i int, table *protocol.PlanningTable, owner *protocol.Reference) (*protocol.PlannedFeatures, error) {
	changes, reason, err := planAlteration(owner, table, p.request.CommonSteps, p.request.Changes, p.request.Capabilities, p.request.TargetFeatures)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return refused(i, reason), nil
	}
	item := alteration{index: i, table: table, change: changes.family, ttl: changes.ttl, partitioning: changes.partitioning}
	if refusal := captureAlteration(owner, changes, &item); refusal != nil {
		return refusal, nil
	}
	if refusal, err := p.planAlterationColumns(owner, table, &item); err != nil || refusal != nil {
		return refusal, err
	}
	p.alterations = append(p.alterations, item)
	return nil, nil
}

// captureAlteration captures the operations of the changes planned for the
// table item alters: each index change and index settings change, its
// changefeed step, and its family, TTL and partitioning operations where each
// changes anything. A change that cannot be captured refuses the request.
func captureAlteration(owner *protocol.Reference, changes alterationChanges, item *alteration) *protocol.PlannedFeatures {
	i := item.index
	var err error
	for _, indexChange := range changes.indexes {
		step := &indexStep{change: indexChange}
		if step.operation, err = CaptureIndexAlteration("pending", owner, indexChange); err != nil {
			return refused(i, err.Error())
		}
		item.indexes = append(item.indexes, step)
	}
	for _, settingsChange := range changes.indexSettings {
		step := &indexSettingsStep{change: settingsChange}
		if step.operation, err = CaptureIndexSettings("pending", owner, settingsChange); err != nil {
			return refused(i, err.Error())
		}
		item.indexSettings = append(item.indexSettings, step)
	}
	var reason string
	if item.feeds, reason = captureChangefeedStep(owner, changes.changefeeds); reason != "" {
		return refused(i, reason)
	}
	if !item.change.empty() {
		if item.operation, err = CaptureAlteration("pending", owner, item.change); err != nil {
			return refused(i, err.Error())
		}
	}
	if !item.ttl.empty() {
		if item.ttlOperation, err = CaptureTTLAlteration("pending", owner, item.ttl); err != nil {
			return refused(i, err.Error())
		}
	}
	if !item.partitioning.empty() {
		if item.partitioningOperation, err = CapturePartitioningAlteration("pending", owner, item.partitioning); err != nil {
			return refused(i, err.Error())
		}
	}
	return nil
}

// planAlterationColumns plans the column steps of the table item alters: the
// host's column steps it claims and, where the request targets the columns
// feature, the column changes planColumnFeature plans. It then captures the
// operation of each claim.
func (p *planner) planAlterationColumns(owner *protocol.Reference, table *protocol.PlanningTable, item *alteration) (*protocol.PlannedFeatures, error) {
	var reason string
	var err error
	if item.claims, reason, err = planColumnClaims(owner, table, p.request.CommonSteps); err != nil {
		return nil, err
	}
	if reason != "" {
		return refused(item.index, reason), nil
	}
	if slices.Contains(p.request.TargetFeatures, provider.ColumnsFeature) {
		if refusal, err := p.planColumnFeature(owner, table, item); err != nil || refusal != nil {
			return refusal, err
		}
	}
	for _, claim := range item.claims {
		if claim.operation, err = captureColumns("pending", owner, claim); err != nil {
			return refused(item.index, err.Error()), nil
		}
	}
	return nil, nil
}

// planColumnFeature plans the column inventory, the default changes and the
// sequence changes of the table item alters, and holds them, with the claims,
// to a complete column alteration.
func (p *planner) planColumnFeature(owner *protocol.Reference, table *protocol.PlanningTable, item *alteration) (*protocol.PlannedFeatures, error) {
	capabilities := p.request.Capabilities
	var reason string
	var err error
	item.inventory, reason, err = planInventory(owner, table, item.claims, p.request.CommonSteps, capabilities)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return refused(item.index, reason), nil
	}
	item.defaults, reason, err = planDefaults(owner, table, capabilities)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return refused(item.index, reason), nil
	}
	item.sequences, reason, err = planSequences(owner, table, capabilities)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return refused(item.index, reason), nil
	}
	if reason := completeColumnAlteration(owner, table, item.claims, item.defaults, item.inventory, item.sequences, capabilities); reason != "" {
		return refused(item.index, reason), nil
	}
	return nil, nil
}

// planTableCreation captures the creation of table i, a row table or a column
// table, with the changefeeds and the vector and full-text indexes it
// declares.
func (p *planner) planTableCreation(i int, table *protocol.PlanningTable, owner *protocol.Reference) (*protocol.PlannedFeatures, error) {
	capabilities := p.request.Capabilities
	if table.Current != nil {
		return nil, fmt.Errorf("YDB table creation requires established absence")
	}
	capture := Capture
	if IsColumnTable(table.Desired) {
		capture = CaptureColumnTable
	}
	operation, err := capture("pending", owner, table.Desired, capabilities)
	if err != nil {
		return refused(i, err.Error()), nil
	}
	if isColumnCreation(operation) {
		reason, err := columnCreationRefusal(owner, operation, capabilities)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return refused(i, reason), nil
		}
	}
	feeds, reason, err := plannedCreationChangefeeds(owner, table.Desired, capabilities)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return refused(i, reason), nil
	}
	indexes, reason, err := plannedCreationIndexes(owner, table.Desired, operation, capabilities)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return refused(i, reason), nil
	}
	settings, reason, err := plannedCreationIndexSettings(owner, table.Desired, operation)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return refused(i, reason), nil
	}
	p.creations = append(p.creations, creation{index: i, table: table, operation: operation, feeds: feeds, indexes: indexes, settings: settings})
	return nil, nil
}

// columnCreationRefusal says why the target cannot take the column table
// operation creates: a TTL tier that moves rows, or a local index whose
// capability the target lacks. It is empty when the target takes the table.
func columnCreationRefusal(owner *protocol.Reference, operation *protocol.NormalizedValue, capabilities map[string]bool) (string, error) {
	created, err := ColumnTable(operation.Value)
	if err != nil {
		return "", err
	}
	if reason := tieredTTLRefusal(capabilities, fmt.Sprintf("column table %q", tableLabel(owner)), created.Ttl); reason != "" {
		return reason, nil
	}
	for _, index := range created.LocalIndexes {
		if reason := localIndexCapabilityRefusal(index, capabilities); reason != "" {
			return reason, nil
		}
	}
	return "", nil
}

// contributeCreations numbers the creations in subject order, the row tables
// and the column tables apart, and contributes the step of each with the steps
// of the indexes and the changefeeds it declares. A creation whose path
// overlaps another table of the request is refused.
func (p *planner) contributeCreations() (*protocol.PlannedFeatures, error) {
	slices.SortFunc(p.creations, func(a, b creation) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	var rowCreations, columnCreations, creationIndexNumber int
	for i := range p.creations {
		item := &p.creations[i]
		prefix, number := "row-table-create", &rowCreations
		if isColumnCreation(item.operation) {
			prefix, number = "column-table-create", &columnCreations
		}
		item.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", prefix, *number)}
		*number++
		item.operation.Id = item.id.Name
		if refusal := p.overlapRefusal(item); refusal != nil {
			return refusal, nil
		}
		step, err := creationStep(item)
		if err != nil {
			return nil, err
		}
		p.contribution.Steps = append(p.contribution.Steps, step)
		p.receiptSteps[item.index] = []*protocol.StepIdentity{item.id}
		for _, index := range item.indexes {
			step, dependency := index.creationStep(creationIndexNumber, item.table.Subject, item.id)
			creationIndexNumber++
			p.contribution.Steps = append(p.contribution.Steps, step)
			p.contribution.Dependencies = append(p.contribution.Dependencies, dependency)
			p.receiptSteps[item.index] = append(p.receiptSteps[item.index], index.id)
		}
		for _, settings := range item.settings {
			step := settings.planningStep(p.indexSettingsNumber, item.table.Subject)
			p.indexSettingsNumber++
			p.contribution.Steps = append(p.contribution.Steps, step)
			p.contribution.Dependencies = append(p.contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(item.id), After: proto.CloneOf(settings.id)})
			p.receiptSteps[item.index] = append(p.receiptSteps[item.index], settings.id)
		}
		if item.feeds != nil {
			step, dependencies := item.feeds.planningStep(p.feedNumber, item.table.Subject, []*protocol.StepIdentity{item.id})
			p.feedNumber++
			p.contribution.Steps = append(p.contribution.Steps, step)
			p.contribution.Dependencies = append(p.contribution.Dependencies, dependencies...)
			p.receiptSteps[item.index] = append(p.receiptSteps[item.index], item.feeds.id)
		}
	}
	return nil, nil
}

// overlapRefusal refuses a creation whose path overlaps the path of another
// table of the request, which needs a directory lifecycle this service does
// not capture.
func (p *planner) overlapRefusal(item *creation) *protocol.PlannedFeatures {
	for _, other := range p.request.Tables {
		if provider.CompareIdentities(other.Subject, item.table.Subject) == 0 {
			continue
		}
		if err := disjoint(item.table.Subject, other.Subject); err != nil {
			return refused(item.index, err.Error())
		}
	}
	return nil
}

// creationStep is the lifecycle step of a creation: it creates the table, its
// scheme path and, for a row table, every index the request builds with it.
func creationStep(item *creation) (*protocol.PlanningStep, error) {
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
	return &protocol.PlanningStep{Id: item.id,
		Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.operation, Phase: protocol.PlanningOperation_LIFECYCLE},
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
			Effects: effects}}, nil
}

// contributeDrops numbers the removals in subject order and contributes the
// step of each, which drops the table and its scheme path. A column table
// whose TTL moves rows to external data sources also reads them, as an early
// reader, as the host's embedded engine drops it: YDB refuses to drop or
// replace a data source a table's TTL names, so the table goes before the
// source's owner drops or replaces the source.
func (p *planner) contributeDrops() error {
	slices.SortFunc(p.drops, func(a, b removal) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	for number := range p.drops {
		item := &p.drops[number]
		item.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("table-drop/%06d", number)}
		item.operation.Id = item.id.Name
		facts := item.operation.OperationSemantics
		slot := proto.CloneOf(item.table.Subject)
		slot.Kind = "ydb.scheme-path"
		held, err := heldTTL(item.table)
		if err != nil {
			return err
		}
		owner, err := boundOwner(item.table.Subject)
		if err != nil {
			return err
		}
		reads := sourceReads(cmp.Or(p.request.DatabasePath, owner.Database), held)
		placement := protocol.PlanningMetadata_DEFAULT
		if len(reads) != 0 {
			placement = protocol.PlanningMetadata_EARLY
		}
		p.contribution.Steps = append(p.contribution.Steps, &protocol.PlanningStep{Id: item.id,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.operation, Phase: protocol.PlanningOperation_LIFECYCLE},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Placement: placement,
				Effects: append([]*protocol.PlanningEffect{{Subject: proto.CloneOf(item.table.Subject), Action: protocol.PlanningEffect_DROP},
					{Subject: slot, Action: protocol.PlanningEffect_DROP}}, reads...)}})
		p.receiptSteps[item.index] = []*protocol.StepIdentity{item.id}
	}
	return nil
}

// contributeRebuilds contributes the steps of each rebuild, each after the
// one before it.
func (p *planner) contributeRebuilds() {
	for _, item := range p.rebuilds {
		var ids []*protocol.StepIdentity
		for n, step := range item.steps {
			p.contribution.Steps = append(p.contribution.Steps, step)
			ids = append(ids, proto.CloneOf(step.Id))
			if n > 0 {
				p.contribution.Dependencies = append(p.contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(item.steps[n-1].Id), After: proto.CloneOf(step.Id)})
			}
		}
		p.receiptSteps[item.index] = ids
	}
}

// alterationNumbers number the steps of each kind the altered tables take,
// across the tables in subject order.
type alterationNumbers struct {
	families, additions, removals, ttls, partitionings int
	indexAdditions, indexRemovals, indexRenames        int
	defaults, inventories, sequences                   int
}

// contributeAlterations contributes the steps of the altered tables in subject
// order.
func (p *planner) contributeAlterations() {
	slices.SortFunc(p.alterations, func(a, b alteration) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	var numbers alterationNumbers
	for i := range p.alterations {
		p.contributeAlteration(&p.alterations[i], &numbers)
	}
}

// contributeAlteration contributes the steps of one altered table, and names
// them in its receipt: the column additions, the default and sequence changes
// of the columns it keeps, its family, TTL and partitioning operations, its
// index changes, its index settings changes, its changefeed step, and the
// column removals.
func (p *planner) contributeAlteration(item *alteration, numbers *alterationNumbers) {
	added, dropped := p.contributeColumnSteps(item, numbers)
	var ordered []*protocol.StepIdentity
	ordered = append(ordered, added...)
	kept := p.contributeKeptColumns(item, numbers)
	ordered = append(ordered, kept...)
	ordered = append(ordered, p.contributeTableSettings(item, numbers)...)
	ordered = append(ordered, p.contributeIndexChanges(item, numbers)...)
	ordered = append(ordered, p.contributeIndexSettings(item)...)
	if item.feeds != nil {
		step, dependencies := item.feeds.planningStep(p.feedNumber, item.table.Subject, nil)
		p.feedNumber++
		p.contribution.Steps = append(p.contribution.Steps, step)
		p.contribution.Dependencies = append(p.contribution.Dependencies, dependencies...)
		ordered = append(ordered, item.feeds.id)
	}
	ordered = append(ordered, dropped...)
	// The column changes run in sequence: the additions, then the default
	// and sequence changes of the columns the table keeps, then the
	// removals. The table's other steps stay out of the sequence. Each
	// carries its own edges to the column steps it depends on, and the
	// host places it: a step that replaces a host step takes that step's
	// place in the host's sequence, and any other step joins the window of
	// its phase. A sequence through them would contradict that order and
	// close a cycle, since the host drops an index before it adds a
	// column, and adds a common index before a TTL changes.
	if len(item.inventory)+len(item.defaults)+len(item.sequences) > 0 {
		columns := slices.Concat(added, kept, dropped)
		for n := 1; n < len(columns); n++ {
			p.contribution.Dependencies = append(p.contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(columns[n-1]), After: proto.CloneOf(columns[n])})
		}
	}
	p.receiptSteps[item.index] = ordered
}

// contributeColumnSteps contributes the native step of each host column step
// the table's alteration claims, with the rewrite that replaces the host's
// step, then the step of each column inventory change, with the rewrite of the
// host step it replaces, and returns the column additions and removals among
// them.
func (p *planner) contributeColumnSteps(item *alteration, numbers *alterationNumbers) (added, dropped []*protocol.StepIdentity) {
	for _, claim := range item.claims {
		prefix, number := "row-table-drop-column", &numbers.removals
		if claim.add {
			prefix, number = "row-table-add-column", &numbers.additions
		}
		claim.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", prefix, *number)}
		*number++
		claim.operation.Id = claim.id.Name
		facts := claim.operation.OperationSemantics
		p.contribution.Steps = append(p.contribution.Steps, &protocol.PlanningStep{Id: claim.id,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: claim.operation},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
				Placement: claim.source.GetMetadata().GetPlacement(), Effects: cloneEffects(claim.source.GetMetadata().GetEffects())}})
		p.result.Rewrites = append(p.result.Rewrites, &protocol.PlanningRewrite{Sources: []*protocol.StepIdentity{proto.CloneOf(claim.source.Id)}, Replacement: proto.CloneOf(claim.id)})
		if claim.add {
			added = append(added, claim.id)
		} else {
			dropped = append(dropped, claim.id)
		}
	}
	for _, change := range item.inventory {
		step := change.plan(item.table.Subject, numbers.inventories)
		if change.source != nil {
			p.result.Rewrites = append(p.result.Rewrites, &protocol.PlanningRewrite{Sources: []*protocol.StepIdentity{proto.CloneOf(change.source.Id)}, Replacement: proto.CloneOf(step.Id)})
		}
		numbers.inventories++
		p.contribution.Steps = append(p.contribution.Steps, step)
		if change.add {
			added = append(added, step.Id)
		} else {
			dropped = append(dropped, step.Id)
		}
	}
	return added, dropped
}

// contributeKeptColumns contributes the default changes, then the sequence
// changes, of the columns the altered table keeps, and returns their steps in
// that order.
func (p *planner) contributeKeptColumns(item *alteration, numbers *alterationNumbers) []*protocol.StepIdentity {
	var kept []*protocol.StepIdentity
	for _, change := range item.defaults {
		step := change.plan(item.table.Subject, numbers.defaults)
		numbers.defaults++
		p.contribution.Steps = append(p.contribution.Steps, step)
		kept = append(kept, step.Id)
	}
	for _, change := range item.sequences {
		step := change.plan(item.table.Subject, numbers.sequences)
		numbers.sequences++
		p.contribution.Steps = append(p.contribution.Steps, step)
		kept = append(kept, step.Id)
	}
	return kept
}

// contributeTableSettings contributes the family, the TTL and the
// partitioning operations of the altered table, each that was captured, with
// the dependencies of the family and the TTL operations on the table's column
// steps, and returns their steps in that order.
func (p *planner) contributeTableSettings(item *alteration, numbers *alterationNumbers) []*protocol.StepIdentity {
	subject := item.table.Subject
	var ordered []*protocol.StepIdentity
	if item.operation != nil {
		item.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-alter/%06d", numbers.families)}
		numbers.families++
		item.operation.Id = item.id.Name
		facts := item.operation.OperationSemantics
		familySlot := proto.CloneOf(subject)
		familySlot.Kind = "ydb.column-families"
		p.contribution.Steps = append(p.contribution.Steps, &protocol.PlanningStep{Id: item.id,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.operation, Phase: protocol.PlanningOperation_LIFECYCLE},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ},
					{Subject: familySlot, Action: protocol.PlanningEffect_ALTER}}}})
		p.contribution.Dependencies = append(p.contribution.Dependencies, alterStepDependencies(*item, p.request.CommonSteps)...)
		ordered = append(ordered, item.id)
	}
	if item.ttlOperation != nil {
		item.ttlID = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-ttl/%06d", numbers.ttls)}
		numbers.ttls++
		item.ttlOperation.Id = item.ttlID.Name
		facts := item.ttlOperation.OperationSemantics
		ttlSlot := proto.CloneOf(subject)
		ttlSlot.Kind = "ydb.ttl"
		p.contribution.Steps = append(p.contribution.Steps, &protocol.PlanningStep{Id: item.ttlID,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.ttlOperation},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ},
					{Subject: ttlSlot, Action: ttlAction(item.ttl)}}}})
		p.contribution.Dependencies = append(p.contribution.Dependencies, ttlStepDependencies(*item, p.request.CommonSteps)...)
		ordered = append(ordered, item.ttlID)
	}
	if item.partitioningOperation != nil {
		item.partitioningID = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("row-table-partitioning/%06d", numbers.partitionings)}
		numbers.partitionings++
		item.partitioningOperation.Id = item.partitioningID.Name
		facts := item.partitioningOperation.OperationSemantics
		partitioningSlot := proto.CloneOf(subject)
		partitioningSlot.Kind = "ydb.partitioning"
		p.contribution.Steps = append(p.contribution.Steps, &protocol.PlanningStep{Id: item.partitioningID,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: item.partitioningOperation},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ},
					{Subject: partitioningSlot, Action: protocol.PlanningEffect_ALTER}}}})
		ordered = append(ordered, item.partitioningID)
	}
	return ordered
}

// contributeIndexChanges contributes the step of each index change of the
// altered table, numbered by its kind, with the rewrite of the host step it
// replaces and its dependencies on the table's column steps, then the order
// between a removal and the addition that replaces it, and returns the steps.
func (p *planner) contributeIndexChanges(item *alteration, numbers *alterationNumbers) []*protocol.StepIdentity {
	subject := item.table.Subject
	var ordered []*protocol.StepIdentity
	for _, index := range item.indexes {
		prefix, number, action := "row-table-drop-index", &numbers.indexRemovals, protocol.PlanningEffect_DROP
		switch {
		case index.change.add != nil:
			prefix, number, action = "row-table-add-index", &numbers.indexAdditions, protocol.PlanningEffect_CREATE
		case index.change.rename != nil:
			prefix, number = "row-table-rename-index", &numbers.indexRenames
		}
		index.id = &protocol.StepIdentity{Owner: schema.ProviderID, Name: fmt.Sprintf("%s/%06d", prefix, *number)}
		*number++
		index.operation.Id = index.id.Name
		facts := index.operation.OperationSemantics
		effects := []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_READ},
			{Subject: indexSubject(subject, index.change.name()), Action: action}}
		if source := index.change.source; source != nil {
			// The replacement changes the index the host's step names: a
			// rename drops the held name and creates the declared one.
			effects = cloneEffects(source.GetMetadata().GetEffects())
			p.result.Rewrites = append(p.result.Rewrites, &protocol.PlanningRewrite{Sources: []*protocol.StepIdentity{proto.CloneOf(source.Id)}, Replacement: proto.CloneOf(index.id)})
		}
		// The lifecycle phase lets a removal and the addition that
		// replaces it under the same name be ordered in one window.
		p.contribution.Steps = append(p.contribution.Steps, &protocol.PlanningStep{Id: index.id,
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: index.operation, Phase: protocol.PlanningOperation_LIFECYCLE},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason,
				Effects: effects}})
		p.contribution.Dependencies = append(p.contribution.Dependencies, indexStepDependencies(*item, index, p.request.CommonSteps)...)
		ordered = append(ordered, index.id)
	}
	p.contribution.Dependencies = append(p.contribution.Dependencies, indexReplacements(item.indexes)...)
	return ordered
}

// contributeIndexSettings contributes the step of each change of the settings
// of an index the altered table holds, then the order that puts each after the
// addition of its index, and returns the steps.
func (p *planner) contributeIndexSettings(item *alteration) []*protocol.StepIdentity {
	subject := item.table.Subject
	var ordered []*protocol.StepIdentity
	for _, settings := range item.indexSettings {
		p.contribution.Steps = append(p.contribution.Steps, settings.planningStep(p.indexSettingsNumber, subject))
		p.indexSettingsNumber++
		ordered = append(ordered, settings.id)
	}
	p.contribution.Dependencies = append(p.contribution.Dependencies, settingsAfterAdditions(item.indexes, item.indexSettings)...)
	return ordered
}

// contributeColumnTables contributes the steps of the altered column tables in
// subject order. A change that cannot be planned refuses the request.
func (p *planner) contributeColumnTables() *protocol.PlannedFeatures {
	slices.SortFunc(p.columnChanges, func(a, b columnTableStep) int { return provider.CompareIdentities(a.table.Subject, b.table.Subject) })
	var localIndexes localIndexNumbers
	for number, item := range p.columnChanges {
		steps, dependencies, err := item.plan(number, &localIndexes)
		if err != nil {
			return refused(item.index, err.Error())
		}
		p.contribution.Steps = append(p.contribution.Steps, steps...)
		p.contribution.Dependencies = append(p.contribution.Dependencies, dependencies...)
		for _, step := range steps {
			p.receiptSteps[item.index] = append(p.receiptSteps[item.index], step.Id)
		}
	}
	return nil
}

// rewriteCommonSteps settles each host step of the request. The host keeps
// what this service only orders against, a rebuild or a removal takes every
// step of its table, and the creation of a table this service creates
// replaces the host's creation of it.
func (p *planner) rewriteCommonSteps() (*protocol.PlannedFeatures, error) {
	// This service can replace only a complete common table creation. A column
	// step or a mixed footprint needs its own lifecycle, never a partial rewrite.
	claimed := make(map[int]bool)
	for _, step := range p.request.CommonSteps {
		if p.settleCommonStep(step) {
			continue
		}
		if refusal, err := p.rewriteCreation(step, claimed); err != nil || refusal != nil {
			return refusal, err
		}
	}
	return nil, nil
}

// settleCommonStep reports whether a host step needs no rewrite of a creation:
// the host keeps it, or a rebuild or a removal of its table takes it.
func (p *planner) settleCommonStep(step *protocol.CommonPlanningStep) bool {
	if step.GetCommonObject() != nil {
		// A view or a comment is the host's step, which this service
		// orders against its own and never takes over.
		return true
	}
	if step.Parent == nil && step.AddedColumn == nil && len(step.GetMetadata().GetEffects()) == 0 {
		// An ordering marker such as common/prerequisite-creations belongs to no
		// table and changes nothing. The host keeps it.
		return true
	}
	if slices.ContainsFunc(p.alterations, func(item alteration) bool { return provider.CompareIdentities(step.Parent, item.table.Subject) == 0 }) {
		// The host keeps the column and setting steps of a table this service only alters.
		return true
	}
	if rebuilt := slices.IndexFunc(p.rebuilds, func(item *rebuild) bool { return provider.CompareIdentities(step.Parent, item.table.Subject) == 0 }); rebuilt >= 0 {
		// The new table takes every change the host would make to the old
		// one: it is written from the declaration.
		p.rebuilds[rebuilt].absorb(step)
		return true
	}
	if dropped := slices.IndexFunc(p.drops, func(item removal) bool { return provider.CompareIdentities(step.Parent, item.table.Subject) == 0 }); dropped >= 0 {
		// The table's DROP TABLE takes everything the host would drop of
		// it: the table, and the indexes and changefeeds it holds.
		p.drops[dropped].sources = append(p.drops[dropped].sources, proto.CloneOf(step.Id))
		return true
	}
	return false
}

// rewriteCreation rewrites the host step that creates a table this service
// creates to that creation. claimed are the creations a host step was
// rewritten to already, and each takes one.
func (p *planner) rewriteCreation(step *protocol.CommonPlanningStep, claimed map[int]bool) (*protocol.PlannedFeatures, error) {
	matched := slices.IndexFunc(p.creations, func(item creation) bool { return provider.CompareIdentities(step.Parent, item.table.Subject) == 0 })
	if matched < 0 {
		if len(p.creations) == 0 {
			return nil, fmt.Errorf("common steps have no captured table creation")
		}
		return refused(p.creations[0].index, "A common step is outside the supported table creation lifecycle"), nil
	}
	item := p.creations[matched]
	effects := step.GetMetadata().GetEffects()
	if claimed[matched] || step.AddedColumn != nil || len(effects) == 0 ||
		effects[0].Action != protocol.PlanningEffect_CREATE || provider.CompareIdentities(effects[0].Subject, item.table.Subject) != 0 ||
		slices.ContainsFunc(effects[1:], func(effect *protocol.PlanningEffect) bool { return !createsIndexOf(effect, item.table.Subject) }) {
		return refused(item.index, "A table creation rewrite requires one complete common table footprint"), nil
	}
	claimed[matched] = true
	p.result.Rewrites = append(p.result.Rewrites, &protocol.PlanningRewrite{Sources: []*protocol.StepIdentity{proto.CloneOf(step.Id)}, Replacement: proto.CloneOf(item.id)})
	return nil, nil
}

// rewriteReplacedTables rewrites the host steps a rebuild takes to the step
// that swaps the new table into place, or to the one that moves the old table
// out of the way (see rebuild.rewrites), and the host steps a removal takes to
// the removal. A rebuild that cannot take its table's steps refuses the
// request.
func (p *planner) rewriteReplacedTables() *protocol.PlannedFeatures {
	for _, item := range p.rebuilds {
		rewrites, reason := item.rewrites()
		if reason != "" {
			return refused(item.index, fmt.Sprintf("rebuilding table %q: %s", tableLabel(item.owner), reason))
		}
		p.result.Rewrites = append(p.result.Rewrites, rewrites...)
	}
	for _, item := range p.drops {
		if len(item.sources) != 0 {
			p.result.Rewrites = append(p.result.Rewrites, &protocol.PlanningRewrite{Sources: item.sources, Replacement: proto.CloneOf(item.id)})
		}
	}
	return nil
}

// receiveChanges answers each table property change of the request with the
// steps of its table. A change whose table has no captured alteration refuses
// the request.
func (p *planner) receiveChanges() *protocol.PlannedFeatures {
	for i, change := range p.request.Changes {
		index := changeIndex(p.request, change)
		if index < 0 {
			return refusedChange(i, "A table property change needs the captured alteration of its table")
		}
		p.result.Changes = append(p.result.Changes, changeReceipt(change, p.receiptSteps[index]))
	}
	return nil
}

// receiveTables answers each parent kind of each table the request plans with
// the strategy and the steps of the table.
func (p *planner) receiveTables() {
	for i, table := range p.request.Tables {
		if table.Action == protocol.PlanningTable_CONTEXT {
			continue
		}
		var steps []*protocol.StepIdentity
		for _, id := range p.receiptSteps[i] {
			steps = append(steps, proto.CloneOf(id))
		}
		strategy := p.strategy(i, table, steps)
		for _, kind := range p.request.ParentKinds {
			p.result.Parents = append(p.result.Parents, &protocol.PlanningParentReceipt{Action: table.Action, Receipt: &protocol.PlanningReceipt{
				Subject: proto.CloneOf(table.Subject), Kind: kind, Strategy: strategy, Steps: steps}})
		}
	}
}

// strategy says how the plan carries out table i of the request, whose
// receipt names steps.
func (p *planner) strategy(i int, table *protocol.PlanningTable, steps []*protocol.StepIdentity) string {
	strategy := "Create the complete captured row table through the native table service, then add its changefeeds with YQL's ALTER TABLE through the query service"
	if slices.Contains(p.columnAlterations, i) {
		strategy = "The column table already holds the declared storage: its kind, its hash key and its shard count are fixed when YDB creates it, and its local indexes and its TTL are the declared ones"
		if len(p.receiptSteps[i]) != 0 {
			strategy = "Drop and add the column table's local indexes, and set its declared TTL in place of the one it holds, resetting a held TTL that moves rows " +
				"to an external data source first, with YQL's ALTER TABLE through the query service; " +
				"its kind, its hash key and its shard count are fixed when YDB creates it and already match"
		}
	}
	if table.Action == protocol.PlanningTable_DROP {
		strategy = "Drop the table, with its rows, indexes and changefeeds, through YQL's DROP TABLE, once the table still holds the columns and the key the plan read"
	}
	if slices.ContainsFunc(p.rebuilds, func(item *rebuild) bool { return item.index == i }) {
		strategy = "Rebuild the table, which YDB cannot change in place: create it anew from the declaration under a scratch name, copy the rows, swap the two tables by renaming them, and drop the old one"
	}
	if slices.ContainsFunc(p.creations, func(item creation) bool { return item.index == i && isColumnCreation(item.operation) }) {
		strategy = "Create the complete captured column table with YQL's CREATE TABLE through the query service, as the host's embedded engine writes it"
	}
	if table.Action == protocol.PlanningTable_ALTER && !slices.Contains(p.columnAlterations, i) {
		strategy = "Add and drop columns, alter the column families and their column placements, set or drop the TTL, " +
			"change the automatic partitioning settings, the read replicas and the key bloom filter, and add or drop global secondary indexes, through the native table service; " +
			"add, drop and change changefeeds with YQL's ALTER TABLE and ALTER TOPIC through the query service; other settings and column steps the native call cannot state stay with the host"
		if steps == nil {
			strategy = "The column families, column placements, TTL, partitioning settings, read replicas, key bloom filter, indexes and changefeeds already match the declaration, and the host has no column step this service can carry out"
		}
	}
	return strategy
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
	// #nosec G115 -- index is a position in the request, which runtime.MaxMessageBytes bounds to 4 MiB
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
