package tablecreate

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// How this provider rebuilds a table.
//
// YDB cannot change a table's key, a column's type or a column's nullability
// toward NOT NULL in place. When the host's planning policy asks for it
// (contract 25.3, `--allow-table-rebuild` on the native surface), the planner
// makes such a change by recreating the table, as the host's embedded engine
// does:
//
//  1. CREATE TABLE a scratch table from the declaration, its indexes with it,
//     each setting the declaration leaves out taken from the old table; then
//     ALTER INDEX ... SET on the scratch table for each index whose held
//     partitioning or read replicas differ from a new index's, since the
//     native call that creates a table takes no index settings;
//  2. INSERT INTO the scratch table the rows of the old one, each changed
//     column converted, a row that does not convert failing the copy;
//  3. ALTER TABLE the old table RENAME TO a second scratch name;
//  4. ALTER TABLE the scratch table RENAME TO the table's name;
//  5. DROP TABLE the renamed old table.
//
// YDB has no transactional DDL and no statement that swaps two tables, so the
// steps are not atomic. Each is one unit, so an interrupted rebuild resumes at
// the step that did not run, and the old rows survive until the last one. The
// copy is one data query, which commits whole or not at all. Rows written to
// the old table between the copy and the swap are not in the new one.
//
// YDB moves no table that carries a changefeed (`Cannot move table with cdc
// streams`), so a rebuild of a table that holds changefeeds drops them before
// step 3 and gives the new table its changefeeds after step 4, as the host's
// embedded engine does: the ones the declaration states, or the ones the old
// table held where the declaration does not state them. Each stream starts
// again, and its consumers lose their positions. A changefeed an asynchronous
// replication controls, or one the read cannot represent, is refused.

// rebuildPrefix and replacedPrefix name the scratch tables of a rebuild, as
// the host's embedded engine names them.
const (
	rebuildPrefix  = "__ptah_rebuild_"
	replacedPrefix = "__ptah_replaced_"
	// rebuildNameAttempts bounds the search for a free scratch name.
	rebuildNameAttempts = 100
)

// rebuildRequest is the words a refusal names a rebuild request with when the
// host did not pass its own.
const rebuildRequest = "a table rebuild request in the host's planning policy (contract 25.3)"

// rebuildNeed is a change of a table YDB makes only by rebuilding it.
type rebuildNeed struct {
	// subject names the change, as the refusal words it.
	subject string
	// capability is the host capability key a target would need to make it in
	// place, and empty for a change no key decides, which subject says why
	// YDB cannot make in place.
	capability string
}

// rebuildNeeds returns the changes of an altered row table that need a
// rebuild: its key, a column's type, a column becoming NOT NULL, a starting
// layout the table was not created with (see layoutRefusal). The declaration
// is compiled with the creation rules; one that does not compile needs no
// rebuild here, and the alteration refuses it in its own words.
func rebuildNeeds(owner *protocol.Reference, table *protocol.PlanningTable, caps map[string]bool) ([]rebuildNeed, *Ydb_Table.CreateTableRequest) {
	desired, err := Compile(owner, table.GetDesired(), caps)
	if err != nil {
		return nil, nil
	}
	label := tableLabel(owner)
	var needs []rebuildNeed
	if held := heldKey(table.GetCurrent()); len(held) != 0 && !slices.Equal(held, desired.GetPrimaryKey()) {
		needs = append(needs, rebuildNeed{capability: "primary_key_alterable",
			subject: fmt.Sprintf("changing the primary key of table %q (%s -> %s)", label, strings.Join(held, ", "), strings.Join(desired.GetPrimaryKey(), ", "))})
	}
	current := table.GetCurrent().GetColumns()
	for _, column := range desired.GetColumns() {
		at := slices.IndexFunc(current, func(held *protocol.ObservedColumn) bool { return held.GetName() == column.GetName() })
		if at < 0 {
			continue
		}
		held := current[at]
		want, wantNullable, err := schema.ColumnType(column.GetType())
		if err != nil {
			continue
		}
		have := heldType(held)
		if serial := SerialTypeOf(column); serial != "" || held.GetAutoIncrement() {
			// A column that becomes or stops being a Serial changes type, as
			// the host's comparison reads it.
			want, have = serialSpelling(want, serial != ""), serialSpelling(have, held.GetAutoIncrement())
		}
		switch {
		case have != "" && have != want:
			needs = append(needs, rebuildNeed{capability: "alter_column_type",
				subject: fmt.Sprintf("changing the type of column %q of table %q (%s -> %s)", column.GetName(), label, have, want)})
		case held.GetIsNullable() == "YES" && !wantNullable && !slices.Contains(desired.GetPrimaryKey(), column.GetName()):
			// A key column's nullability is not compared: YDB makes a key
			// column nullable unless it says otherwise, the declaration writes
			// every key column NOT NULL, and the host's embedded engine reads
			// no change into the difference.
			needs = append(needs, rebuildNeed{capability: "alter_column_set_not_null",
				subject: fmt.Sprintf("making column %q of table %q NOT NULL", column.GetName(), label)})
		}
	}
	// A layout's witness is the minimum it gives a new table, which a rebuild
	// creates the table with: keepHeldSettings keeps no held minimum beside
	// a layout.
	if wanted, err := layoutOfRequest(desired); err == nil {
		if reason := layoutRefusal(wanted, desired.GetPartitioningSettings(), heldProperties(table.GetCurrent()).GetPartitioningSettings()); reason != "" {
			needs = append(needs, rebuildNeed{subject: fmt.Sprintf("table %q: %s", label, reason)})
		}
	}
	return needs, desired
}

// rebuildRefusal is the refusal of a change only a rebuild makes, when the
// plan was not asked to rebuild: the embedded engine's words, naming how the
// operator asks for one.
func rebuildRefusal(need rebuildNeed, policy *protocol.PlanningPolicy) string {
	if need.capability == "" {
		return fmt.Sprintf("%s; YDB makes it by rebuilding the table, which Ptah plans when asked with %s", need.subject, requestedWith(policy))
	}
	return fmt.Sprintf("%s, which requires target capability %s, unavailable on this ydb target; YDB makes it by rebuilding the table, which Ptah plans when asked with %s",
		need.subject, need.capability, requestedWith(policy))
}

// requestedWith is how the operator asks for a table rebuild: what the host
// names in its policy, or the policy field itself for a host that names
// nothing.
func requestedWith(policy *protocol.PlanningPolicy) string {
	if request := policy.GetTableRebuildRequest(); request != "" {
		return request
	}
	return rebuildRequest
}

// keepHeldComments gives the new table of a rebuild the comments the read
// reported on the old one, its own and those of the columns and indexes the
// new table has, as the attributes YDB keeps them in. On the common wire a
// comment is not a table setting, so the read of the table's properties
// holds none, and the host plans no change for a comment it finds in place.
// The host carries them on the observed table, its columns and its indexes;
// the planning request's observed common objects hold what lies beyond the
// tables, and a host fills them for no table.
func keepHeldComments(request *Ydb_Table.CreateTableRequest, current *protocol.ObservedTable) {
	comments := ydbcomment.Comments{Own: current.GetComment(), Columns: map[string]string{}, Indexes: map[string]string{}}
	for _, column := range current.GetColumns() {
		if column.GetComment() != "" && slices.ContainsFunc(request.GetColumns(), func(created *Ydb_Table.ColumnMeta) bool { return created.GetName() == column.GetName() }) {
			comments.Columns[column.GetName()] = column.GetComment()
		}
	}
	for _, index := range current.GetIndexes() {
		if index.GetComment() != "" && slices.ContainsFunc(request.GetIndexes(), func(created *Ydb_Table.TableIndex) bool { return created.GetName() == index.GetName() }) {
			comments.Indexes[index.GetName()] = index.GetComment()
		}
	}
	for key, value := range comments.Attributes() {
		if request.Attributes == nil {
			request.Attributes = map[string]string{}
		}
		request.Attributes[key] = value
	}
}

// heldKey is the key of an observed table, in key order where the read of its
// properties states it and in column order otherwise.
func heldKey(observed *protocol.ObservedTable) []string {
	var key []string
	for _, column := range observed.GetColumns() {
		if column.GetPrimaryKey() {
			key = append(key, column.GetName())
		}
	}
	if properties := heldProperties(observed); len(properties.GetPrimaryKey()) != 0 {
		return slices.Clone(properties.GetPrimaryKey())
	}
	return key
}

// heldProperties is the read of an observed table's properties, empty when
// the read holds none.
func heldProperties(observed *protocol.ObservedTable) *native.DescribeTableResult {
	var held native.DescribeTableResult
	for _, record := range observed.GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() == schema.TableProperties {
			if err := protojson.Unmarshal(record.GetValue().GetValue().GetPayload(), &held); err == nil {
				return &held
			}
		}
	}
	return &held
}

// heldColumns are the column properties of an observed table by column name.
func heldColumnProperties(observed *protocol.ObservedTable) map[string]*native.ColumnMeta {
	result := map[string]*native.ColumnMeta{}
	for _, record := range observed.GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() != schema.ColumnProperties {
			continue
		}
		var column native.ColumnMeta
		if err := protojson.Unmarshal(record.GetValue().GetValue().GetPayload(), &column); err == nil {
			result[record.GetValue().GetValue().GetOwner().GetName()] = &column
		}
	}
	return result
}

// heldType is the YDB type of an observed column as the provider spells it,
// or "" when the read does not say.
func heldType(column *protocol.ObservedColumn) string {
	kind, err := schema.StorageType(column.GetDataType(), false)
	if err != nil {
		return ""
	}
	name, _, err := schema.ColumnType(kind)
	if err != nil {
		return ""
	}
	return name
}

// rebuild is one table the plan recreates, and the steps it takes.
type rebuild struct {
	index int
	// swap is the step that moves the new table into place, which a common
	// step on the table is rewritten to.
	swap     int
	table    *protocol.PlanningTable
	owner    *protocol.Reference
	steps    []*protocol.PlanningStep
	sources  []*protocol.StepIdentity
	scratch  string
	replaced string
}

// planRebuild builds the steps of a rebuild of the table owner names into
// the declaration desired compiles to, numbered with number, or the reason it
// cannot be rebuilt. names are the tables the request declares or holds, which
// a scratch name must not take. common is what the read reported of the common
// objects, whose comments on the table the new one keeps. steps are the host's
// steps, whose index renames name the old index a new one takes its settings
// from.
func planRebuild(number int, owner *protocol.Reference, table *protocol.PlanningTable, desired *Ydb_Table.CreateTableRequest, names map[string]bool,
	capabilities map[string]bool, steps []*protocol.CommonPlanningStep) (*rebuild, string, error) {
	label := tableLabel(owner)
	subject := fmt.Sprintf("rebuilding table %q", label)
	current := table.GetCurrent()
	if reason := serialRebuildRefusal(desired); reason != "" {
		return nil, subject + ": " + reason, nil
	}
	feeds, reason, err := rebuildChangefeeds(owner, table, desired, capabilities)
	if err != nil {
		return nil, "", err
	}
	if reason != "" {
		return nil, subject + ": " + reason, nil
	}
	if name := rebuiltNativeIndex(table); name != "" {
		return nil, subject + fmt.Sprintf(": the table declares or holds the vector or full-text index %q, which the rebuild does not create; "+
			"drop it, rebuild, and add it again", name), nil
	}
	scratch, err := freeName(owner, rebuildPrefix, names)
	if err != nil {
		return nil, subject + ": " + err.Error(), nil
	}
	replaced, err := freeName(owner, replacedPrefix, names)
	if err != nil {
		return nil, subject + ": " + err.Error(), nil
	}
	request := proto.CloneOf(desired)
	keepHeldSettings(request, table)
	keepHeldComments(request, table.GetCurrent())
	indexSettings, reason := rebuiltIndexSettings(request, table, steps)
	if reason != "" {
		return nil, subject + ": " + reason, nil
	}
	heldColumns := make([]string, 0, len(current.GetColumns()))
	for _, column := range current.GetColumns() {
		heldColumns = append(heldColumns, column.GetName())
	}
	heldKeyColumns := heldKey(current)
	copied, reason := copiedColumns(request, current)
	if reason != "" {
		return nil, subject + ": " + reason, nil
	}
	scratchOwner, replacedOwner := renamed(owner, scratch), renamed(owner, replaced)
	newColumns := make([]string, 0, len(request.GetColumns()))
	for _, column := range request.GetColumns() {
		newColumns = append(newColumns, column.GetName())
	}
	prefix := fmt.Sprintf("table-rebuild/%06d/", number)
	create, err := CaptureRequest(prefix+"create", scratchOwner, request)
	if err != nil {
		return nil, subject + ": " + err.Error(), nil
	}
	copyOperation, err := CaptureCopy(prefix+"copy", scratchOwner, Copy{Source: owner.GetName(), Columns: copied})
	if err != nil {
		return nil, "", err
	}
	moveOld, err := CaptureRename(prefix+"rename-old", owner, Rename{Target: replaced, Columns: heldColumns, Key: heldKeyColumns})
	if err != nil {
		return nil, "", err
	}
	moveNew, err := CaptureRename(prefix+"rename-new", scratchOwner, Rename{Target: owner.GetName(), Columns: newColumns, Key: slices.Clone(request.GetPrimaryKey())})
	if err != nil {
		return nil, "", err
	}
	drop, err := CaptureDropShape(prefix+"drop-old", replacedOwner, Drop{Store: RowStore, Columns: heldColumns, Key: heldKeyColumns})
	if err != nil {
		return nil, "", err
	}
	subjectOf := func(name string) *protocol.ComparisonIdentity {
		identity := proto.CloneOf(table.GetSubject())
		identity.Name = &protocol.ComparisonPart{Source: name, Normalized: name}
		return identity
	}
	slot := func(identity *protocol.ComparisonIdentity) *protocol.ComparisonIdentity {
		result := proto.CloneOf(identity)
		result.Kind = "ydb.scheme-path"
		return result
	}
	self, scratchSubject, replacedSubject := table.GetSubject(), subjectOf(scratch), subjectOf(replaced)
	type rebuildStep struct {
		operation *protocol.NormalizedValue
		effects   []*protocol.PlanningEffect
		notes     []string
	}
	planned := []rebuildStep{
		{operation: create, effects: []*protocol.PlanningEffect{{Subject: scratchSubject, Action: protocol.PlanningEffect_CREATE},
			{Subject: slot(scratchSubject), Action: protocol.PlanningEffect_CREATE}}, notes: rebuildNotes(label)},
	}
	// The settings of an index change on the empty new table, before the copy
	// fills it.
	for n, change := range indexSettings {
		operation, err := CaptureIndexSettings(fmt.Sprintf("%sindex-settings/%06d", prefix, n), scratchOwner, change)
		if err != nil {
			return nil, subject + ": " + err.Error(), nil
		}
		planned = append(planned, rebuildStep{operation: operation, effects: []*protocol.PlanningEffect{
			{Subject: proto.CloneOf(scratchSubject), Action: protocol.PlanningEffect_READ},
			{Subject: indexSubject(scratchSubject, change.held.GetName()), Action: protocol.PlanningEffect_ALTER}}})
	}
	planned = append(planned, rebuildStep{operation: copyOperation, effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(self), Action: protocol.PlanningEffect_READ},
		{Subject: proto.CloneOf(scratchSubject), Action: protocol.PlanningEffect_ALTER}}})
	if len(feeds.drops) != 0 {
		operation, err := schema.CaptureChangefeedOperation(prefix+"drop-changefeeds", owner, feeds.drops)
		if err != nil {
			return nil, subject + ": " + err.Error(), nil
		}
		planned = append(planned, rebuildStep{operation: operation, effects: changefeedEffects(self, feeds.drops, protocol.PlanningEffect_DROP), notes: feeds.notes})
	}
	planned = append(planned,
		rebuildStep{operation: moveOld, effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(self), Action: protocol.PlanningEffect_ALTER},
			{Subject: replacedSubject, Action: protocol.PlanningEffect_CREATE}, {Subject: slot(replacedSubject), Action: protocol.PlanningEffect_CREATE}}},
		rebuildStep{operation: moveNew, effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(scratchSubject), Action: protocol.PlanningEffect_DROP},
			{Subject: slot(scratchSubject), Action: protocol.PlanningEffect_DROP}, {Subject: proto.CloneOf(self), Action: protocol.PlanningEffect_ALTER}}})
	result := &rebuild{owner: owner, table: table, scratch: scratch, replaced: replaced, swap: len(planned) - 1}
	if len(feeds.adds) != 0 {
		operation, err := schema.CaptureChangefeedOperation(prefix+"add-changefeeds", owner, feeds.adds)
		if err != nil {
			return nil, subject + ": " + err.Error(), nil
		}
		planned = append(planned, rebuildStep{operation: operation, effects: changefeedEffects(self, feeds.adds, protocol.PlanningEffect_CREATE)})
	}
	planned = append(planned, rebuildStep{operation: drop, effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(replacedSubject), Action: protocol.PlanningEffect_DROP},
		{Subject: slot(replacedSubject), Action: protocol.PlanningEffect_DROP}}})
	for _, step := range planned {
		facts := step.operation.OperationSemantics
		result.steps = append(result.steps, &protocol.PlanningStep{Id: &protocol.StepIdentity{Owner: schema.ProviderID, Name: step.operation.Id},
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: step.operation, Phase: protocol.PlanningOperation_LIFECYCLE,
				Notes: step.notes},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: step.effects}})
	}
	return result, "", nil
}

// changefeedEffects is the footprint of a rebuild step that changes the
// changefeeds of table: it reads the table and takes action on each
// changefeed changes names.
func changefeedEffects(table *protocol.ComparisonIdentity, changes []changefeeds.Change, action protocol.PlanningEffect_Action) []*protocol.PlanningEffect {
	effects := []*protocol.PlanningEffect{{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ}}
	for _, change := range changes {
		effects = append(effects, &protocol.PlanningEffect{Subject: changefeedSubject(table, change.Name), Action: action})
	}
	return effects
}

// rebuiltChangefeeds are the changefeed changes of a rebuild: the ones that
// take every changefeed off the old table before it moves, the ones that give
// the new table its changefeeds once it is in place, and the notes that say
// what the operator loses.
type rebuiltChangefeeds struct {
	drops, adds []changefeeds.Change
	notes       []string
}

// rebuildChangefeeds returns the changefeed changes of a rebuild of the table
// owner names into the declaration desired compiles to, or the reason the
// rebuild cannot carry the table's changefeeds. The new table gets the
// changefeeds the declaration states, and the ones the old table held where
// the declaration does not state them.
func rebuildChangefeeds(owner *protocol.Reference, table *protocol.PlanningTable, desired *Ydb_Table.CreateTableRequest, capabilities map[string]bool) (rebuiltChangefeeds, string, error) {
	declared, err := facetEnvelopes(owner, table.GetDesired().GetFeatures().GetFacets(), protocol.ValueInput_DESIRED)
	if err != nil {
		return rebuiltChangefeeds{}, "", err
	}
	observed, err := facetEnvelopes(owner, table.GetCurrent().GetFeatures().GetFacets(), protocol.ValueInput_OBSERVED)
	if err != nil {
		return rebuiltChangefeeds{}, "", err
	}
	_, held, err := changefeedSets(nil, observed.changefeeds)
	if err != nil {
		return rebuiltChangefeeds{}, "", err
	}
	var result rebuiltChangefeeds
	label := tableLabel(owner)
	if len(held.GetChangefeeds()) != 0 {
		drops, undecided := changefeeds.Diff(&native.ChangefeedSetIntent{}, held)
		if undecided != "" {
			return rebuiltChangefeeds{}, undecided, nil
		}
		// Diff keeps a stream an asynchronous replication controls, which the
		// old table then still carries when it moves.
		if len(drops) != len(held.GetChangefeeds()) {
			return rebuiltChangefeeds{}, "the table holds a changefeed an asynchronous replication controls, and YDB moves no table that carries one " +
				"(`Cannot move table with cdc streams`); the rebuild cannot drop it and add it again", nil
		}
		for _, change := range drops {
			feed, err := changefeeds.FromObserved(change.Before)
			if err != nil {
				return rebuiltChangefeeds{}, "", err
			}
			result.notes = append(result.notes, changefeeds.RebuiltNote(label, feed))
		}
		result.drops = drops
	}
	wanted := declared.changefeeds
	if !changefeedsManaged(table, declared, observed) && observed.changefeeds != nil {
		if wanted, err = schema.ConvertNative(observed.changefeeds, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED); err != nil {
			return rebuiltChangefeeds{}, "", err
		}
	}
	indexes := make([]string, 0, len(desired.GetIndexes()))
	for _, index := range desired.GetIndexes() {
		indexes = append(indexes, index.GetName())
	}
	adds, reason, err := planChangefeeds(changefeedInput{owner: owner, managed: true, desired: wanted, indexes: indexes,
		keyType: firstKeyType(table.GetDesired()), capabilities: capabilities})
	if err != nil || reason != "" {
		return rebuiltChangefeeds{}, reason, err
	}
	result.adds = adds
	return result, "", nil
}

// rebuiltIndexSettings returns the changes that give each global index of the
// new table of a rebuild the partitioning and read replicas the old table's
// index holds, as the host's embedded engine names every setting of a rebuilt
// index: the declaration states no setting of an index the new table takes
// from the common declaration, so each one keeps what the old index holds. The
// old index is the one under the same name, or under the name the host's step
// renames it from, as the read describes it: in the native index set, or, for
// an index the common model describes, beside it. An index the old table does not hold, or whose settings
// are a new index's, needs no change. The native call that creates the table
// takes no index settings, so each change runs on the new table after it is
// created, over the settings YDB gives a new index (see newIndexDescription).
// A non-empty reason refuses the rebuild.
func rebuiltIndexSettings(request *Ydb_Table.CreateTableRequest, table *protocol.PlanningTable, steps []*protocol.CommonPlanningStep) ([]indexSettingsChange, string) {
	var held native.IndexObservations
	for _, record := range table.GetCurrent().GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() == schema.IndexProperties {
			if err := protojson.Unmarshal(record.GetValue().GetValue().GetPayload(), &held); err != nil {
				return nil, fmt.Sprintf("the read of the table's indexes does not decode: %v", err)
			}
		}
	}
	formerName := map[string]string{}
	for _, step := range steps {
		if rename := step.GetRenamedIndex(); rename != nil && step.GetParent() != nil && provider.CompareIdentities(step.GetParent(), table.GetSubject()) == 0 {
			formerName[rename.GetTo()] = rename.GetFrom()
		}
	}
	var changes []indexSettingsChange
	for _, index := range request.GetIndexes() {
		name := index.GetName()
		if former, renamed := formerName[name]; renamed {
			name = former
		}
		var old *native.TableIndexDescription
		if at := slices.IndexFunc(held.GetIndexes(), func(observed *native.ObservedIndex) bool { return observed.GetDescription().GetName() == name }); at >= 0 {
			old = held.GetIndexes()[at].GetDescription()
		}
		if at := slices.IndexFunc(held.GetCommonIndexes(), func(observed *native.TableIndexDescription) bool { return observed.GetName() == name }); at >= 0 {
			old = held.GetCommonIndexes()[at]
		}
		if old == nil {
			continue
		}
		change, reason := planIndexSettings(index.GetName(), keptIndexSettings(HeldIndexSettings(old)), newIndexDescription(indexIntentOf(index)))
		if reason != "" {
			return nil, reason
		}
		if change != nil {
			changes = append(changes, *change)
		}
	}
	return changes, ""
}

// keptIndexSettings is what a new index states to keep the settings held
// describes: every automatic partitioning setting, the partition size only
// where it splits by size, since YDB reports none otherwise, and the read
// replicas. The partition columns and a starting layout are left out: YDB
// partitions an index on its own key and takes no layout for one.
func keptIndexSettings(held *native.GlobalIndexSettings) *native.GlobalIndexSettings {
	partitioning := proto.CloneOf(held.GetPartitioningSettings())
	if partitioning != nil {
		partitioning.PartitionBy = nil
		if partitioning.GetPartitioningBySize() != Ydb.FeatureFlag_ENABLED {
			partitioning.PartitionSizeMb = 0
		}
	}
	return &native.GlobalIndexSettings{PartitioningSettings: partitioning, ReadReplicasSettings: proto.CloneOf(held.GetReadReplicasSettings())}
}

// rebuildNotes are the notes the first step of a rebuild carries above it:
// what the rebuild does, and what it cannot keep.
func rebuildNotes(label string) []string {
	return []string{
		fmt.Sprintf("Rebuild of table %s: YDB cannot make this change in place, so the table is created anew, the rows are copied, and the two tables are swapped.", label),
		fmt.Sprintf("The steps are not atomic. Rows written to %s between the copy and the swap are lost, and YDB has no lock to stop them: stop writing to the table until the last step has run.", label),
		"The copy is one query. YDB refuses one that carries more than about 48 MiB on 25.1 or 64 MiB on 26.2; nothing is then copied, and the old table keeps serving.",
	}
}

// renamed is owner with another name in its directory.
func renamed(owner *protocol.Reference, name string) *protocol.Reference {
	result := proto.CloneOf(owner)
	result.Name = name
	return result
}

// freeName picks a scratch name for owner under prefix that no table of the
// request takes.
func freeName(owner *protocol.Reference, prefix string, names map[string]bool) (string, error) {
	base := prefix + owner.GetName()
	for attempt := range rebuildNameAttempts {
		candidate := base
		if attempt > 0 {
			candidate = base + "_" + strconv.Itoa(attempt)
		}
		if !names[TextPath(owner.GetSchema(), candidate)] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no scratch table name is free: %s and %d numbered variants are all taken", base, rebuildNameAttempts-1)
}

// copiedColumns are the columns the copy writes: every column of the new table
// the old one holds, converted where its type changes and unwrapped where the
// new table holds it NOT NULL and the old value may be NULL or not convert. A
// column the new table adds is left to its default.
func copiedColumns(request *Ydb_Table.CreateTableRequest, current *protocol.ObservedTable) ([]CopiedColumn, string) {
	var copied []CopiedColumn
	for _, column := range request.GetColumns() {
		at := slices.IndexFunc(current.GetColumns(), func(held *protocol.ObservedColumn) bool { return held.GetName() == column.GetName() })
		if at < 0 {
			if column.GetNotNull() && column.GetDefaultValue() == nil {
				return nil, fmt.Sprintf("the copy has no value for column %q, which the new table adds NOT NULL without a default: give it a default or make it nullable", column.GetName())
			}
			continue
		}
		held := current.GetColumns()[at]
		want, _, err := schema.ColumnType(column.GetType())
		if err != nil {
			return nil, err.Error()
		}
		entry := CopiedColumn{Name: column.GetName()}
		if have := heldType(held); have != "" && have != want {
			entry.Cast = want
		}
		key := slices.Contains(request.GetPrimaryKey(), column.GetName())
		becomesNotNull := held.GetIsNullable() == "YES" && column.GetNotNull()
		entry.Required = column.GetNotNull() && (entry.Cast != "" || becomesNotNull || key)
		copied = append(copied, entry)
	}
	if len(copied) == 0 {
		return nil, "the new table keeps none of the old table's columns, so there is nothing to copy and every row would be lost"
	}
	return copied, ""
}

// keepHeldSettings gives the new table of a rebuild every setting the
// declaration leaves out as the old table holds it: every family the old
// table holds, the default one included, with each setting the declaration
// does not state, the family each column sits in where the declaration does
// not say, the automatic partitioning, read replicas, key bloom filter and
// user attributes, and the TTL where the declaration states no table
// properties at all. A declaration says where a column sits with a column
// property record, and a source that describes where every column sits says
// it of a column it places in no family too: that column sits in the default
// family (see PlacedInDefault). A cluster's
// table profile may have given the old table a family or a setting no
// declaration names, and YQL drops neither, so a rebuild that left one out
// would change the table where nobody asked. Storage settings nobody declared
// are the server's, which a new table takes too: YDB reports its defaults,
// and YQL has no setting to write them with.
func keepHeldSettings(request *Ydb_Table.CreateTableRequest, table *protocol.PlanningTable) {
	current, desired := table.GetCurrent(), table.GetDesired()
	held := heldProperties(current)
	stated := slices.ContainsFunc(desired.GetFeatures().GetFacets().GetRecords(), func(record *protocol.ComparisonRecord) bool {
		return record.GetKind() == schema.TableProperties
	})
	if !stated && request.TtlSettings == nil {
		request.TtlSettings = proto.CloneOf(held.GetTtlSettings())
	}
	partitioning := request.GetPartitioningSettings()
	if partitioning == nil {
		partitioning = &Ydb_Table.PartitioningSettings{}
	}
	heldPartitioning := held.GetPartitioningSettings()
	if partitioning.PartitioningBySize == Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		partitioning.PartitioningBySize = heldPartitioning.GetPartitioningBySize()
	}
	if partitioning.PartitionSizeMb == 0 && partitioning.PartitioningBySize == Ydb.FeatureFlag_ENABLED {
		partitioning.PartitionSizeMb = heldPartitioning.GetPartitionSizeMb()
	}
	if partitioning.PartitioningByLoad == Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		partitioning.PartitioningByLoad = heldPartitioning.GetPartitioningByLoad()
	}
	if partitioning.MinPartitionsCount == 0 && request.GetPartitions() == nil {
		partitioning.MinPartitionsCount = heldPartitioning.GetMinPartitionsCount()
	}
	if partitioning.MaxPartitionsCount == 0 {
		partitioning.MaxPartitionsCount = heldPartitioning.GetMaxPartitionsCount()
	}
	if proto.Size(partitioning) != 0 {
		request.PartitioningSettings = partitioning
	}
	if request.ReadReplicasSettings == nil {
		request.ReadReplicasSettings = proto.CloneOf(held.GetReadReplicasSettings())
	}
	if request.KeyBloomFilter == Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		request.KeyBloomFilter = held.GetKeyBloomFilter()
	}
	for key, value := range held.GetAttributes() {
		if _, declared := request.GetAttributes()[key]; !declared {
			if request.Attributes == nil {
				request.Attributes = map[string]string{}
			}
			request.Attributes[key] = value
		}
	}
	placements := heldColumnProperties(current)
	families := map[string]bool{}
	for _, family := range request.GetColumnFamilies() {
		families[familyKey(family.GetName())] = true
		if held := findFamily(held.GetColumnFamilies(), family.GetName()); held != nil {
			if family.GetData().GetMedia() == "" && held.GetData().GetMedia() != "" {
				family.Data = proto.CloneOf(held.GetData())
			}
			if family.GetCompression() == Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED {
				family.Compression = held.GetCompression()
			}
		}
	}
	for _, family := range held.GetColumnFamilies() {
		if families[familyKey(family.GetName())] {
			continue
		}
		request.ColumnFamilies = append(request.ColumnFamilies, &Ydb_Table.ColumnFamily{Name: familyKey(family.GetName()), Data: proto.CloneOf(family.GetData()),
			Compression: family.GetCompression()})
		families[familyKey(family.GetName())] = true
	}
	placed := map[string]bool{}
	for _, record := range desired.GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() == schema.ColumnProperties && record.GetValue() != nil {
			placed[record.GetSubject().GetName().GetNormalized()] = true
		}
	}
	for _, column := range request.GetColumns() {
		if column.GetFamily() != "" || placed[column.GetName()] ||
			PlacedInDefault(desired.GetFeatures().GetFacets().GetCoverage(), ColumnSubject(table.GetSubject(), column.GetName())) {
			continue
		}
		place := placements[column.GetName()].GetFamily()
		if place == "" || familyKey(place) == DefaultFamily {
			continue
		}
		if families[familyKey(place)] {
			column.Family = place
		}
	}
}

// requestTableNames are the paths of every table a planning request declares
// or holds, which a scratch name must not take.
func requestTableNames(request *protocol.PlanFeaturesRequest) map[string]bool {
	names := map[string]bool{}
	for _, table := range request.GetTables() {
		names[TextPath(table.GetSubject().GetSchema().GetNormalized(), table.GetSubject().GetName().GetNormalized())] = true
	}
	return names
}
