package tablecreate

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
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
//     each setting the declaration leaves out taken from the old table;
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
// A table that holds changefeeds is refused: YDB moves no table that carries
// one (`Cannot move table with cdc streams`), and the changefeeds would have to
// be dropped and added again around the swap.

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
	// place.
	capability string
}

// rebuildNeeds returns the changes of an altered row table that need a
// rebuild: its key, a column's type, a column becoming NOT NULL. The
// declaration is compiled with the creation rules; one that does not compile
// needs no rebuild here, and the alteration refuses it in its own words.
func rebuildNeeds(owner *protocol.Reference, table *protocol.PlanningTable) ([]rebuildNeed, *Ydb_Table.CreateTableRequest) {
	desired, err := Compile(owner, table.GetDesired())
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
	return needs, desired
}

// rebuildRefusal is the refusal of a change only a rebuild makes, when the
// plan was not asked to rebuild: the embedded engine's words, naming how the
// operator asks for one.
func rebuildRefusal(need rebuildNeed, policy *protocol.PlanningPolicy) string {
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
func keepHeldComments(request *Ydb_Table.CreateTableRequest, owner *protocol.Reference, common *protocol.ObservedCommonObjects) {
	comments := ydbcomment.Comments{Columns: map[string]string{}, Indexes: map[string]string{}}
	for _, comment := range common.GetComments() {
		object := comment.GetObject()
		switch {
		case object.GetSchema() != owner.GetSchema():
		case object.GetKind() == "table" && object.GetName() == owner.GetName():
			comments.Own = comment.GetText()
		case len(object.GetPath()) != 1 || object.GetPath()[0] != owner.GetName():
		case object.GetKind() == "column" && slices.ContainsFunc(request.GetColumns(), func(column *Ydb_Table.ColumnMeta) bool { return column.GetName() == object.GetName() }):
			comments.Columns[object.GetName()] = comment.GetText()
		case object.GetKind() == "index" && slices.ContainsFunc(request.GetIndexes(), func(index *Ydb_Table.TableIndex) bool { return index.GetName() == object.GetName() }):
			comments.Indexes[object.GetName()] = comment.GetText()
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
	index    int
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
// objects, whose comments on the table the new one keeps.
func planRebuild(number int, owner *protocol.Reference, table *protocol.PlanningTable, desired *Ydb_Table.CreateTableRequest, names map[string]bool,
	common *protocol.ObservedCommonObjects) (*rebuild, string, error) {
	label := tableLabel(owner)
	subject := fmt.Sprintf("rebuilding table %q", label)
	current := table.GetCurrent()
	for _, record := range current.GetFeatures().GetFacets().GetRecords() {
		if record.GetKind() != schema.ChangefeedProperties {
			continue
		}
		var held native.ChangefeedObservations
		if err := protojson.Unmarshal(record.GetValue().GetValue().GetPayload(), &held); err == nil && len(held.GetChangefeeds()) != 0 {
			return nil, subject + ": the table holds changefeeds, and YDB moves no table that carries one (`Cannot move table with cdc streams`); " +
				"drop them, rebuild, and add them again", nil
		}
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
	keepHeldSettings(request, current, table.GetDesired())
	keepHeldComments(request, owner, common)
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
	effects := [][]*protocol.PlanningEffect{
		{{Subject: scratchSubject, Action: protocol.PlanningEffect_CREATE}, {Subject: slot(scratchSubject), Action: protocol.PlanningEffect_CREATE}},
		{{Subject: proto.CloneOf(self), Action: protocol.PlanningEffect_READ}, {Subject: proto.CloneOf(scratchSubject), Action: protocol.PlanningEffect_ALTER}},
		{{Subject: proto.CloneOf(self), Action: protocol.PlanningEffect_ALTER}, {Subject: replacedSubject, Action: protocol.PlanningEffect_CREATE},
			{Subject: slot(replacedSubject), Action: protocol.PlanningEffect_CREATE}},
		{{Subject: proto.CloneOf(scratchSubject), Action: protocol.PlanningEffect_DROP}, {Subject: slot(scratchSubject), Action: protocol.PlanningEffect_DROP},
			{Subject: proto.CloneOf(self), Action: protocol.PlanningEffect_ALTER}},
		{{Subject: proto.CloneOf(replacedSubject), Action: protocol.PlanningEffect_DROP}, {Subject: slot(replacedSubject), Action: protocol.PlanningEffect_DROP}},
	}
	result := &rebuild{owner: owner, table: table, scratch: scratch, replaced: replaced}
	for i, operation := range []*protocol.NormalizedValue{create, copyOperation, moveOld, moveNew, drop} {
		facts := operation.OperationSemantics
		result.steps = append(result.steps, &protocol.PlanningStep{Id: &protocol.StepIdentity{Owner: schema.ProviderID, Name: operation.Id},
			Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: operation, Phase: protocol.PlanningOperation_LIFECYCLE,
				Notes: rebuildNotes(label, i)},
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: facts.Impact, Reason: facts.Reason, Effects: effects[i]}})
	}
	return result, "", nil
}

// rebuildNotes are the notes the first step of a rebuild carries above it:
// what the rebuild does, and what it cannot keep.
func rebuildNotes(label string, step int) []string {
	if step != 0 {
		return nil
	}
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
// declaration leaves out as the old table holds it: the families and the
// family each column sits in, the automatic partitioning, read replicas, key
// bloom filter and user attributes, and the TTL where the declaration states
// no table properties at all. Storage settings nobody declared are the
// server's, which a new table takes too: YDB reports its defaults, and YQL
// has no setting to write them with.
func keepHeldSettings(request *Ydb_Table.CreateTableRequest, current *protocol.ObservedTable, desired *protocol.DeclaredTable) {
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
	for _, column := range request.GetColumns() {
		if column.GetFamily() != "" {
			continue
		}
		place := placements[column.GetName()].GetFamily()
		if place == "" || familyKey(place) == DefaultFamily {
			continue
		}
		if !families[familyKey(place)] {
			if family := findFamily(held.GetColumnFamilies(), place); family != nil {
				request.ColumnFamilies = append(request.ColumnFamilies, &Ydb_Table.ColumnFamily{Name: family.GetName(), Data: proto.CloneOf(family.GetData()),
					Compression: family.GetCompression()})
				families[familyKey(place)] = true
			}
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
