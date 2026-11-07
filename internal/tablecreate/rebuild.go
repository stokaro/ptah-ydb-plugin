package tablecreate

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
//     ALTER INDEX ... SET on the scratch table for each index whose declared
//     or held partitioning or read replicas differ from a new index's, since
//     the native call that creates a table takes no index settings;
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
// replication controls, or one the read cannot represent, is refused, and so is
// a table whose read does not describe its changefeeds (see
// undescribedChangefeeds).

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
		needs = append(needs, rebuildNeed{capability: "primary_key_alterable", subject: keyChange(label, table.GetCurrent(), held, desired.GetPrimaryKey())})
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
		// A column that becomes or stops being a Serial changes type, as the
		// host's comparison reads it.
		want, have := declaredSpelling(want, column), heldSpelling(heldType(held), held)
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

// keyChange names the change of a table's primary key from held to declared.
// A key over the same columns in another order is the change the host hands
// over from a read that reports keys, and is named as the host's embedded
// engine named it: the addition of the key under the name the read gives the
// held one. Any other key change names both keys.
func keyChange(label string, current *protocol.ObservedTable, held, declared []string) string {
	if slices.Equal(slices.Sorted(slices.Values(held)), slices.Sorted(slices.Values(declared))) {
		name := current.GetName() + "_pkey"
		for _, constraint := range current.GetConstraints() {
			if strings.EqualFold(constraint.GetType(), "PRIMARY KEY") && constraint.GetName() != "" {
				name = constraint.GetName()
			}
		}
		return fmt.Sprintf("adding constraint %s, the primary key", name)
	}
	return fmt.Sprintf("changing the primary key of table %q (%s -> %s)", label, strings.Join(held, ", "), strings.Join(declared, ", "))
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
	comments := ydbcomment.Comments{Own: current.GetComment(), Columns: make(map[string]string), Indexes: make(map[string]string)}
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
			request.Attributes = make(map[string]string)
		}
		request.Attributes[key] = value
	}
}

// heldKey is the key of an observed table: the host's PRIMARY KEY constraint
// of it, else the key the read of its properties states, else its key
// columns in column order.
func heldKey(observed *protocol.ObservedTable) []string {
	// The host's PRIMARY KEY constraint is the key the plan starts from: a
	// read reports it in key order, and the reverse plan of a key change
	// projects the forward change onto it, where the read of the table's
	// properties still holds the key the forward plan found.
	for _, constraint := range observed.GetConstraints() {
		if strings.EqualFold(constraint.GetType(), "PRIMARY KEY") && len(constraint.GetColumnNames()) != 0 {
			return slices.Clone(constraint.GetColumnNames())
		}
	}
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
	result := make(map[string]*native.ColumnMeta)
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
	kind, err := schema.StorageType(column.GetDataType())
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
	// step on the table is rewritten to, and retire the step before it, which
	// moves the old table out of the way (see rewrites).
	swap, retire int
	table        *protocol.PlanningTable
	owner        *protocol.Reference
	steps        []*protocol.PlanningStep
	// absorbed are the host's steps on the table, which the rebuild takes.
	absorbed []*protocol.CommonPlanningStep
	scratch  string
	replaced string
}

// absorb takes the host's step on the rebuilt table into the rebuild: the new
// table is written from the declaration, so it makes every change the host
// would make to the old one. rewrites says what each step is rewritten to.
func (r *rebuild) absorb(step *protocol.CommonPlanningStep) {
	r.absorbed = append(r.absorbed, step)
}

// rewrites rewrites the host's steps the rebuild absorbed to its renames, and
// names on each rename every object the steps rewritten to it change, since
// the host's graph refuses a rewrite whose replacement leaves out an object
// its sources change: an index or a column a step adds or drops, besides the
// table. A step is rewritten to the rename that puts the new table in place.
// An object one step drops and another adds again, such as an index whose
// definition changes, cannot be named twice by one step, so the step that
// drops it is rewritten to the rename before, which moves the old table, and
// the object with it, out of the way. The host orders its steps on a table one
// after the other, its removals first, so every step up to the last such drop
// is rewritten there too, or the renames would have to run in both orders. A
// reason refuses the rebuild where one of those steps adds or changes
// something.
func (r *rebuild) rewrites() ([]*protocol.PlanningRewrite, string) {
	var created []*protocol.ComparisonIdentity
	for _, step := range r.absorbed {
		for _, effect := range step.GetMetadata().GetEffects() {
			if effect.GetAction() == protocol.PlanningEffect_CREATE {
				created = append(created, effect.GetSubject())
			}
		}
	}
	readded := func(effect *protocol.PlanningEffect) bool {
		return effect.GetAction() == protocol.PlanningEffect_DROP && slices.ContainsFunc(created, func(subject *protocol.ComparisonIdentity) bool {
			return provider.CompareIdentities(subject, effect.GetSubject()) == 0
		})
	}
	retired := -1
	for i, step := range r.absorbed {
		if slices.ContainsFunc(step.GetMetadata().GetEffects(), readded) {
			retired = i
		}
	}
	sources := make(map[int][]*protocol.StepIdentity)
	for i, step := range r.absorbed {
		target := r.swap
		if i <= retired {
			if !dropsOnlyParts(step.GetMetadata().GetEffects(), r.table.GetSubject()) {
				return nil, fmt.Sprintf("the host's step %s changes the table before it drops an index or a column the table gains again, "+
					"and the rebuild drops what the old table holds before it puts the new table in place", step.GetId().GetName())
			}
			target = r.retire
		}
		sources[target] = append(sources[target], proto.CloneOf(step.GetId()))
		r.name(target, step.GetMetadata().GetEffects())
	}
	var result []*protocol.PlanningRewrite
	for _, target := range []int{r.retire, r.swap} {
		if len(sources[target]) != 0 {
			result = append(result, &protocol.PlanningRewrite{Sources: sources[target], Replacement: proto.CloneOf(r.steps[target].GetId())})
		}
	}
	return result, ""
}

// dropsOnlyParts reports whether effects drop something of table, such as an
// index or a column, and change nothing else.
func dropsOnlyParts(effects []*protocol.PlanningEffect, table *protocol.ComparisonIdentity) bool {
	dropped := false
	for _, effect := range effects {
		switch {
		case effect.GetAction() == protocol.PlanningEffect_READ:
		case effect.GetAction() != protocol.PlanningEffect_DROP || provider.CompareIdentities(effect.GetSubject(), table) == 0:
			return false
		default:
			dropped = true
		}
	}
	return dropped
}

// name adds to step n of the rebuild each object effects change that it does
// not name yet.
func (r *rebuild) name(n int, effects []*protocol.PlanningEffect) {
	metadata := r.steps[n].GetMetadata()
	for _, effect := range effects {
		if effect.GetAction() == protocol.PlanningEffect_READ {
			continue
		}
		if !slices.ContainsFunc(metadata.GetEffects(), func(held *protocol.PlanningEffect) bool {
			return provider.CompareIdentities(held.GetSubject(), effect.GetSubject()) == 0
		}) {
			metadata.Effects = append(metadata.Effects, proto.CloneOf(effect))
		}
	}
}

// planRebuild builds the steps of a rebuild of the table owner names into
// the declaration desired compiles to, numbered with number, or the reason it
// cannot be rebuilt. names are the tables the request declares or holds, which
// a scratch name must not take. steps are the host's steps, whose index renames
// name the old index a new one takes its settings from, and changes the
// request's changes, whose changes of an index's settings state what the index
// is to hold (see changedIndexSettings).
func planRebuild(number int, owner *protocol.Reference, table *protocol.PlanningTable, desired *Ydb_Table.CreateTableRequest, names,
	capabilities map[string]bool, steps []*protocol.CommonPlanningStep, changes []*protocol.PlanningChange,
) (*rebuild, string, error) {
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
	scratch, replaced, err := rebuildNames(owner, names)
	if err != nil {
		return nil, subject + ": " + err.Error(), nil
	}
	request := proto.CloneOf(desired)
	keepHeldSettings(request, table)
	keepHeldComments(request, table.GetCurrent())
	if reason := rebuiltTableRefusal(subject, table, request, capabilities); reason != "" {
		return nil, reason, nil
	}
	indexSettings, reason, err := rebuiltIndexSettings(owner, request, table, steps, changes)
	if err != nil {
		return nil, "", err
	}
	if reason != "" {
		return nil, subject + ": " + reason, nil
	}
	heldColumns := columnNames(current.GetColumns())
	heldKeyColumns := heldKey(current)
	copied, reason := copiedColumns(request, current)
	if reason != "" {
		return nil, subject + ": " + reason, nil
	}
	scratchOwner, replacedOwner := renamed(owner, scratch), renamed(owner, replaced)
	newColumns := columnNames(request.GetColumns())
	prefix := fmt.Sprintf("table-rebuild/%06d/", number)
	create, err := CaptureRequest(prefix+"create", scratchOwner, request)
	if err != nil {
		return nil, subject + ": " + err.Error(), nil
	}
	grants, grantsNote := regrants(label, table, steps)
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
	drop, err := CaptureDropShape(prefix+"drop-old", replacedOwner, retiredTable(heldColumns, heldKeyColumns, owner.GetName(), grants))
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
	planned := []rebuildStep{
		{operation: create, effects: []*protocol.PlanningEffect{{Subject: scratchSubject, Action: protocol.PlanningEffect_CREATE},
			{Subject: slot(scratchSubject), Action: protocol.PlanningEffect_CREATE}}, notes: slices.Concat(rebuildNotes(label), feeds.notes, grantsNote)},
	}
	// The settings of an index change on the empty new table, before the copy
	// fills it.
	settingsSteps, err := indexSettingsRebuildSteps(prefix, scratchOwner, scratchSubject, indexSettings)
	if err != nil {
		return nil, subject + ": " + err.Error(), nil
	}
	planned = append(planned, settingsSteps...)
	planned = append(planned, rebuildStep{operation: copyOperation, effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(self), Action: protocol.PlanningEffect_READ},
		{Subject: proto.CloneOf(scratchSubject), Action: protocol.PlanningEffect_ALTER}}})
	if len(feeds.drops) != 0 {
		operation, err := schema.CaptureChangefeedOperation(prefix+"drop-changefeeds", owner, feeds.drops)
		if err != nil {
			return nil, subject + ": " + err.Error(), nil
		}
		planned = append(planned, rebuildStep{operation: operation, effects: changefeedEffects(self, feeds.drops, protocol.PlanningEffect_DROP)})
	}
	planned = append(planned,
		rebuildStep{operation: moveOld, effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(self), Action: protocol.PlanningEffect_ALTER},
			{Subject: replacedSubject, Action: protocol.PlanningEffect_CREATE}, {Subject: slot(replacedSubject), Action: protocol.PlanningEffect_CREATE}}},
		rebuildStep{operation: moveNew, effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(scratchSubject), Action: protocol.PlanningEffect_DROP},
			{Subject: slot(scratchSubject), Action: protocol.PlanningEffect_DROP}, {Subject: proto.CloneOf(self), Action: protocol.PlanningEffect_ALTER}}})
	result := &rebuild{owner: owner, table: table, scratch: scratch, replaced: replaced, swap: len(planned) - 1, retire: len(planned) - 2}
	if len(feeds.adds) != 0 {
		operation, err := schema.CaptureChangefeedOperation(prefix+"add-changefeeds", owner, feeds.adds)
		if err != nil {
			return nil, subject + ": " + err.Error(), nil
		}
		planned = append(planned, rebuildStep{operation: operation, effects: changefeedEffects(self, feeds.adds, protocol.PlanningEffect_CREATE)})
	}
	planned = append(planned, rebuildStep{operation: drop, effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(replacedSubject), Action: protocol.PlanningEffect_DROP},
		{Subject: slot(replacedSubject), Action: protocol.PlanningEffect_DROP}}})
	result.steps = rebuildPlanningSteps(planned)
	return result, "", nil
}

// regrants are the grants the new table of a rebuild of table, which label
// names, takes in the TEXT form, after the old table is dropped (see
// DropText): each grant the read found on the held table
// (planning.table-grants, contract 25.33), except a privilege the table holds
// by default rather than by a grant and one the plan revokes, whose REVOKE in
// steps runs after the rebuild, as the host's embedded engine left out an
// entry the plan removes. They are sorted by subject and permission. A host
// that does not send the grants leaves them unknown, and so does a read that
// did not describe them, which the note says: a rebuild run from a migration
// file then gives the new table none, while one Ptah runs copies them from
// the old table when it runs.
func regrants(label string, table *protocol.PlanningTable, steps []*protocol.CommonPlanningStep) ([]TableGrant, []string) {
	held := table.GetCurrentGrants()
	if held == nil {
		return nil, nil
	}
	if !held.GetComplete() {
		return nil, []string{fmt.Sprintf("The read did not describe the grants of table %s, so the rebuild written into a migration file gives the new "+
			"table none of them; run by Ptah, the rebuild copies them from the old table.", label)}
	}
	revoked := make(map[TableGrant]bool)
	for _, step := range steps {
		revoke := step.GetCommonObject().GetRevoke()
		target := revoke.GetTarget()
		if revoke == nil || target.GetKind() != "table" || len(revoke.GetColumns()) != 0 || !sameTable(target, table.GetCurrent()) {
			continue
		}
		for _, privilege := range revoke.GetPrivileges() {
			revoked[TableGrant{Role: revoke.GetRole(), Permission: permissionName(privilege)}] = true
		}
	}
	var grants []TableGrant
	for _, grant := range held.GetGrants() {
		copied := TableGrant{Role: grant.GetRole(), Permission: permissionName(grant.GetPrivilege())}
		if grant.GetImplicit() || revoked[copied] || slices.Contains(grants, copied) {
			continue
		}
		grants = append(grants, copied)
	}
	slices.SortFunc(grants, func(a, b TableGrant) int {
		return cmp.Or(cmp.Compare(a.Role, b.Role), cmp.Compare(a.Permission, b.Permission))
	})
	return grants, nil
}

// retiredTable is the drop of the old table of a rebuild, which holds columns
// and key, with the grants the TEXT form gives the table that takes its name,
// name, after the drop.
func retiredTable(columns, key []string, name string, grants []TableGrant) Drop {
	drop := Drop{Store: RowStore, Columns: columns, Key: key, Grants: grants}
	if len(grants) != 0 {
		drop.GrantsOn = name
	}
	return drop
}

// sameTable reports whether target names the held table current, by its
// directory and name.
func sameTable(target *protocol.Reference, current *protocol.ObservedTable) bool {
	return strings.Trim(target.GetSchema(), "/") == strings.Trim(current.GetSchema(), "/") && target.GetName() == current.GetName()
}

// permissionName is the YDB permission a privilege names, in YDB's spelling,
// or the privilege as it stands where it names none Ptah knows.
func permissionName(privilege string) string {
	if permission, known := ydbacl.Permission(privilege); known {
		return permission
	}
	return privilege
}

// rebuildStep is one operation of a rebuild, with its footprint and the notes
// it carries.
type rebuildStep struct {
	operation *protocol.NormalizedValue
	effects   []*protocol.PlanningEffect
	notes     []string
}

// rebuildNames are the scratch name the new table of a rebuild is created
// under and the name the old table moves to, each free among names.
func rebuildNames(owner *protocol.Reference, names map[string]bool) (scratch, replaced string, err error) {
	if scratch, err = freeName(owner, rebuildPrefix, names); err != nil {
		return "", "", err
	}
	if replaced, err = freeName(owner, replacedPrefix, names); err != nil {
		return "", "", err
	}
	return scratch, replaced, nil
}

// columnNames lists the names of columns in order.
func columnNames[C interface{ GetName() string }](columns []C) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.GetName())
	}
	return names
}

// indexSettingsRebuildSteps are the steps that give each index of the new
// table of a rebuild, which scratchOwner names and scratchSubject identifies,
// the settings changes states, numbered under prefix.
func indexSettingsRebuildSteps(prefix string, scratchOwner *protocol.Reference, scratchSubject *protocol.ComparisonIdentity, changes []indexSettingsChange) ([]rebuildStep, error) {
	var steps []rebuildStep
	for n, change := range changes {
		operation, err := CaptureIndexSettings(fmt.Sprintf("%sindex-settings/%06d", prefix, n), scratchOwner, change)
		if err != nil {
			return nil, err
		}
		steps = append(steps, rebuildStep{operation: operation, effects: []*protocol.PlanningEffect{
			{Subject: proto.CloneOf(scratchSubject), Action: protocol.PlanningEffect_READ},
			{Subject: indexSubject(scratchSubject, change.held.GetName()), Action: protocol.PlanningEffect_ALTER}}})
	}
	return steps, nil
}

// rebuildPlanningSteps are the lifecycle steps of the operations of a
// rebuild, in order, each named by its operation.
func rebuildPlanningSteps(planned []rebuildStep) []*protocol.PlanningStep {
	var steps []*protocol.PlanningStep
	for _, step := range planned {
		facts := step.operation.OperationSemantics
		steps = append(steps, &protocol.PlanningStep{Id: &protocol.StepIdentity{Owner: schema.ProviderID, Name: step.operation.Id},
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: step.operation, Phase: protocol.PlanningOperation_LIFECYCLE,
				Notes: step.notes},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: step.effects}})
	}
	return steps
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
	if reason := undescribedChangefeeds(table); reason != "" {
		return rebuiltChangefeeds{}, reason, nil
	}
	// As the alteration refuses it: without the table's own read, a changefeed
	// the table holds would look absent, and the rename would fail after the
	// new table is created and filled.
	if declared.changefeeds != nil && observed.changefeeds == nil {
		return rebuiltChangefeeds{}, "the table's current changefeeds were not captured, and YDB moves no table that carries one " +
			"(`Cannot move table with cdc streams`)", nil
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
	if result.notes, err = rebuiltChangefeedNotes(label, result.drops, adds); err != nil {
		return rebuiltChangefeeds{}, "", err
	}
	return result, "", nil
}

// rebuiltChangefeedNotes are the notes a rebuild of table carries above its
// first step about the changefeeds the old table holds, as the host's
// embedded engine writes them there: what dropping each one the new table
// does not get loses, then why the ones it gets again are dropped before the
// swap and what their restart costs.
func rebuiltChangefeedNotes(table string, drops, adds []changefeeds.Change) ([]string, error) {
	var notes, restored []string
	for _, change := range drops {
		if slices.ContainsFunc(adds, func(add changefeeds.Change) bool { return add.Name == change.Name }) {
			restored = append(restored, change.Name)
			continue
		}
		feed, err := changefeeds.FromObserved(change.Before)
		if err != nil {
			return nil, err
		}
		notes = append(notes, changefeeds.DroppedNote(table, feed))
	}
	if len(restored) != 0 {
		notes = append(notes, changefeeds.RebuiltNote(table, restored))
	}
	return notes, nil
}

// rebuiltIndexSettings returns the changes that give each global index of the
// new table of a rebuild, which the table owner names declares as request, its
// partitioning and read replicas, as the host's embedded engine names every
// setting of a rebuilt index: each setting the index's own facet declares, and
// for every other one what the old index holds. The old index is the one under
// the same name, or under the name the host's step renames it from, as the read
// describes it: in the native index set, or, for an index the common model
// describes, beside it. An index the old table does not hold takes the
// settings it declares over the ones YDB gives a new index, and one that
// neither declares nor holds settings other than a new index's needs no
// change. The native call that creates the table takes no index settings, so
// each change runs on the new table after it is created, over the settings YDB
// gives a new index (see newIndexDescription). A non-empty reason refuses the
// rebuild; an index facet the declaration states for an index the new table
// does not create is one.
func rebuiltIndexSettings(owner *protocol.Reference, request *Ydb_Table.CreateTableRequest, table *protocol.PlanningTable,
	steps []*protocol.CommonPlanningStep, changes []*protocol.PlanningChange,
) ([]indexSettingsChange, string, error) {
	var held native.IndexObservations
	for _, record := range table.GetCurrent().GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() == schema.IndexProperties {
			if err := protojson.Unmarshal(record.GetValue().GetValue().GetPayload(), &held); err != nil {
				return nil, fmt.Sprintf("the read of the table's indexes does not decode: %v", err), nil
			}
		}
	}
	declared, _, err := indexSettingsFacets(owner, table.GetDesired().GetFeatures().GetFacets(), protocol.ValueInput_DESIRED)
	if err != nil {
		return nil, "", err
	}
	changed, err := changedIndexSettings(table.GetSubject(), changes)
	if err != nil {
		return nil, "", err
	}
	maps.Copy(declared, changed)
	for _, name := range slices.Sorted(maps.Keys(declared)) {
		if !slices.ContainsFunc(request.GetIndexes(), func(index *Ydb_Table.TableIndex) bool { return index.GetName() == name }) {
			return nil, fmt.Sprintf("index %q declares its partitioning or read replicas, and the new table creates no global index of that name", name), nil
		}
	}
	formerName := make(map[string]string)
	for _, step := range steps {
		if rename := step.GetRenamedIndex(); rename != nil && step.GetParent() != nil && provider.CompareIdentities(step.GetParent(), table.GetSubject()) == 0 {
			formerName[rename.GetTo()] = rename.GetFrom()
		}
	}
	var result []indexSettingsChange
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
		var kept *native.GlobalIndexSettings
		if old != nil {
			kept = keptIndexSettings(HeldIndexSettings(old))
		}
		change, reason := planIndexSettings(index.GetName(), declaredOver(declared[index.GetName()], kept), newIndexDescription(indexIntentOf(index)))
		if reason != "" {
			return nil, reason, nil
		}
		if change != nil {
			result = append(result, *change)
		}
	}
	return result, "", nil
}

// declaredOver is what an index of the new table of a rebuild is given: each
// setting stated declares, and the one kept holds for every other, as the
// host's embedded engine resolves a rebuilt index's declaration over the
// settings the old index holds. A size stated without partitioning by size
// turns it on, as YDB does, and an index left without partitioning by size
// keeps no size it did not state. Read replicas stated as none are none. A nil
// stated is kept itself, and a nil kept is an index the old table does not
// hold.
func declaredOver(stated *native.IndexSettingsIntent, kept *native.GlobalIndexSettings) *native.GlobalIndexSettings {
	if stated == nil {
		return kept
	}
	result := proto.CloneOf(kept)
	if result == nil {
		result = &native.GlobalIndexSettings{}
	}
	if partitioning := stated.GetPartitioningSettings(); partitioning != nil {
		merged := ApplyPartitioning(result.GetPartitioningSettings(), withSizeSplitting(partitioning))
		if merged.GetPartitioningBySize() != Ydb.FeatureFlag_ENABLED && partitioning.GetPartitionSizeMb() == 0 {
			merged.PartitionSizeMb = 0
		}
		result.PartitioningSettings = merged
	}
	if replicas := stated.GetReadReplicasSettings(); replicas.GetSettings() != nil {
		result.ReadReplicasSettings = proto.CloneOf(replicas)
	}
	return result
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
// user attributes, the TTL where the declaration states no table properties
// at all, and the TTL's run interval where the declaration states a TTL. A
// declaration says where a column sits with a column
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
	// A declared TTL names no run interval, since YQL has no setting for one,
	// and a table created without one runs its TTL on YDB's default. The new
	// table takes the interval the old one holds, as a TTL changed in place
	// keeps it (see planTTL). The text form refuses such a table (see
	// ttlSetting), so a migration file is refused rather than dropping it.
	if request.GetTtlSettings() != nil && request.TtlSettings.RunIntervalSeconds == 0 {
		request.TtlSettings.RunIntervalSeconds = held.GetTtlSettings().GetRunIntervalSeconds()
	}
	keepHeldPartitioning(request, held)
	if request.ReadReplicasSettings == nil {
		request.ReadReplicasSettings = proto.CloneOf(held.GetReadReplicasSettings())
	}
	if request.KeyBloomFilter == Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		request.KeyBloomFilter = held.GetKeyBloomFilter()
	}
	// A table nobody gave a key bloom filter reads with none stated, which
	// is a filter that is off; the new table names it so, as the host's
	// embedded engine writes every setting of a rebuilt table.
	if request.KeyBloomFilter == Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		request.KeyBloomFilter = Ydb.FeatureFlag_DISABLED
	}
	for key, value := range held.GetAttributes() {
		if _, declared := request.GetAttributes()[key]; !declared {
			if request.Attributes == nil {
				request.Attributes = make(map[string]string)
			}
			request.Attributes[key] = value
		}
	}
	keepHeldStorage(request, held)
	families := keepHeldFamilies(request, held)
	keepHeldPlacements(request, table, families)
}

// rebuiltTableRefusal is why the new table of a rebuild, request with what
// the old table holds, cannot be created, or "". A read that could not
// describe the old table's properties leaves the rebuild nothing to keep
// them from (see undescribedProperties). YDB refuses a new table that
// stores external blobs (`Setting store_external_blobs to ENABLED is not
// allowed`), measured on 25.1.4.7 and 26.2.1.14, so a rebuild cannot keep
// them; and a cache mode the old table holds is written into the new table
// too, so the target must take one.
func rebuiltTableRefusal(subject string, table *protocol.PlanningTable, request *Ydb_Table.CreateTableRequest, capabilities map[string]bool) string {
	if reason := undescribedProperties(table); reason != "" {
		return subject + ": the read could not describe the table's TTL, column families or partitioning (" + reason +
			"), so the new table would drop them; change the table by hand"
	}
	if request.GetStorageSettings().GetStoreExternalBlobs() == Ydb.FeatureFlag_ENABLED {
		return subject + ": the table stores external blobs, which YDB does not let a new table do " +
			"(`Setting store_external_blobs to ENABLED is not allowed`), so the new table would drop them; change the table by hand"
	}
	return creationCacheModeRefusal(subject+" with column family cache mode", request, capabilities)
}

// keepHeldStorage gives the new table of a rebuild each storage setting the
// declaration leaves out as the old table holds it: its tablet's commit log
// pools, its external pool and whether it stores external blobs. CreateTable
// keeps the pools, measured on 25.1.4.7 and 26.2.1.14; a new table that
// stores external blobs is refused when the rebuild is planned (see
// planRebuild). A migration file refuses any of them, since YQL has no
// setting for a row table's storage (see RowTableStatementIn). YDB reads
// external blobs as DISABLED for a table that set nothing, which is no
// setting to keep.
func keepHeldStorage(request *Ydb_Table.CreateTableRequest, held *native.DescribeTableResult) {
	stored := held.GetStorageSettings()
	if stored == nil {
		return
	}
	settings := request.GetStorageSettings()
	if settings == nil {
		settings = &Ydb_Table.StorageSettings{}
	}
	if settings.TabletCommitLog0 == nil {
		settings.TabletCommitLog0 = proto.CloneOf(stored.GetTabletCommitLog0())
	}
	if settings.TabletCommitLog1 == nil {
		settings.TabletCommitLog1 = proto.CloneOf(stored.GetTabletCommitLog1())
	}
	if settings.External == nil {
		settings.External = proto.CloneOf(stored.GetExternal())
	}
	if settings.StoreExternalBlobs == Ydb.FeatureFlag_STATUS_UNSPECIFIED && stored.GetStoreExternalBlobs() == Ydb.FeatureFlag_ENABLED {
		settings.StoreExternalBlobs = Ydb.FeatureFlag_ENABLED
	}
	if !proto.Equal(settings, &Ydb_Table.StorageSettings{}) {
		request.StorageSettings = settings
	}
}

// keepHeldPartitioning gives the new table of a rebuild each automatic
// partitioning setting the declaration leaves out as the old table holds it:
// the partitioning by size, with its partition size where it is enabled, the
// partitioning by load, the minimum partition count where the request states
// no starting layout, and the maximum partition count.
func keepHeldPartitioning(request *Ydb_Table.CreateTableRequest, held *native.DescribeTableResult) {
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
}

// keepHeldFamilies gives the new table of a rebuild every family the old
// table holds, with each setting the declaration does not state as the old
// table holds it, and returns the families the new table takes by key.
func keepHeldFamilies(request *Ydb_Table.CreateTableRequest, held *native.DescribeTableResult) map[string]bool {
	families := make(map[string]bool)
	for i, requested := range request.GetColumnFamilies() {
		families[familyKey(requested.GetName())] = true
		held := findFamily(held.GetColumnFamilies(), requested.GetName())
		family, err := schema.NativeFamily(requested)
		if held == nil || err != nil {
			// A family the request cannot read is refused when the creation
			// is captured.
			continue
		}
		if family.GetData().GetMedia() == "" && held.GetData().GetMedia() != "" {
			family.Data = proto.CloneOf(held.GetData())
		}
		if family.GetCompression() == Ydb_Table.ColumnFamily_COMPRESSION_UNSPECIFIED {
			family.Compression = held.GetCompression()
		}
		if family.GetCacheMode() == native.ColumnFamily_CACHE_MODE_UNSPECIFIED {
			family.CacheMode = held.GetCacheMode()
		}
		if family.GetKeepInMemory() == Ydb.FeatureFlag_STATUS_UNSPECIFIED {
			family.KeepInMemory = held.GetKeepInMemory()
		}
		request.ColumnFamilies[i] = schema.RequestFamily(family)
	}
	// A family the old table keeps in memory is kept there by the native call
	// that creates the new table; a migration file, which YQL has no family
	// setting for, refuses it when it is written (see familySettings).
	for _, family := range held.GetColumnFamilies() {
		if families[familyKey(family.GetName())] {
			continue
		}
		request.ColumnFamilies = append(request.ColumnFamilies, schema.RequestFamily(&native.ColumnFamily{Name: familyKey(family.GetName()),
			Data: proto.CloneOf(family.GetData()), Compression: family.GetCompression(), CacheMode: family.GetCacheMode(), KeepInMemory: family.GetKeepInMemory()}))
		families[familyKey(family.GetName())] = true
	}
	return families
}

// keepHeldPlacements places each column of the new table of a rebuild whose
// placement the declaration does not state in the family the old table holds
// it in, where the new table takes that family and it is not the default one.
func keepHeldPlacements(request *Ydb_Table.CreateTableRequest, table *protocol.PlanningTable, families map[string]bool) {
	desired := table.GetDesired()
	placements := heldColumnProperties(table.GetCurrent())
	placed := make(map[string]bool)
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
	names := make(map[string]bool)
	for _, table := range request.GetTables() {
		names[TextPath(table.GetSubject().GetSchema().GetNormalized(), table.GetSubject().GetName().GetNormalized())] = true
	}
	return names
}

// undescribedProperties is the reason the read of table gives for claiming
// its table properties unrepresentable, such as a column family holding a
// field a newer server writes, or "".
func undescribedProperties(table *protocol.PlanningTable) string {
	for _, claim := range table.GetCurrent().GetFeatures().GetFacets().GetCoverage().GetSubjects() {
		if claim.GetKind() == schema.TableProperties && claim.GetKnowledge().GetState() == protocol.ModelKnowledge_UNREPRESENTABLE &&
			provider.CompareIdentities(claim.GetSubject(), table.GetSubject()) == 0 && forThisEngine(claim.GetTargets()) {
			return cmp.Or(claim.GetKnowledge().GetReason(), "the read gives no reason")
		}
	}
	return ""
}
