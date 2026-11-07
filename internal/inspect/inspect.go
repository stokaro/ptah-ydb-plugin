package inspect

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// directoryRegion is the namespace of one directory of database, below which
// nothing is claimed: a nested directory is a namespace of its own.
func directoryRegion(database, directory string) *protocol.ModelNamespaceKnowledge {
	return &protocol.ModelNamespaceKnowledge{
		Scope: &protocol.ModelNamespace{
			Database: &protocol.NamespaceComponent{Source: database, Normalized: database},
			Catalog:  &protocol.NamespaceComponent{},
			Schema:   &protocol.NamespaceComponent{Source: directory, Normalized: directory},
		},
		Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
	}
}

// The pinned vendor protobuf does not name STREAMING_QUERY.
const streamingQueryEntry = Ydb_Scheme.Entry_Type(26)

// ColumnTables reads the monitoring endpoint's description of the column table
// at an absolute path: the page columnstore.Decode reads.
type ColumnTables func(ctx context.Context, absolute string) ([]byte, error)

// Read captures exact common column fields and native property facets. A
// partial traversal, an object of a kind it does not know, or an incomplete
// native description returns no snapshot. A column store and a sequence, which
// Ptah does not model, and a queue of the older persistent queue kind are
// recorded rather than read, and the read goes on. Filtered schemas are exact
// directories, not recursive prefixes.
//
// columns reads the monitoring description a column table needs beside the
// table service's: the table service leaves out the shard count and the local
// indexes. A nil columns refuses every column table, naming the monitoring
// parameter the address lacks.
//
// leaveOut names the directories at the root that belong to other runs, such
// as the directory of the dev realms in a database a realm does not select.
// Neither the traversal nor a selector reaches them.
func Read(ctx context.Context, driver *ydb.Driver, columns ColumnTables, database string, selectors []string, leaveOut ...string) (*protocol.Schema, error) {
	return ReadCommon(ctx, driver, columns, database, selectors, Common{}, leaveOut...)
}

// ReadCommon is [Read] that also reports the common objects common names,
// each kind complete for the directories the read lists.
func ReadCommon(ctx context.Context, driver *ydb.Driver, columns ColumnTables, database string, selectors []string, common Common, leaveOut ...string) (result *protocol.Schema, err error) {
	return ReadObjects(ctx, driver, columns, database, selectors, common, nil, leaveOut...)
}

// StreamingQuery reads the stored settings of an absolute streaming query
// path. A nil reader leaves this family uninspected.
type StreamingQuery func(context.Context, string) (*streaming.Spec, error)

// ReadObjects includes streaming queries where their capability and system
// view are available. An unread query has explicit incomplete coverage.
func ReadObjects(ctx context.Context, driver *ydb.Driver, columns ColumnTables, database string, selectors []string, common Common, queries StreamingQuery, leaveOut ...string) (result *protocol.Schema, err error) {
	schemas, bindings, err := scopes(database, selectors, leaveOut)
	if err != nil {
		return nil, err
	}
	source, err := open(ctx, driver)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, source.close())
		if err != nil {
			result = nil
		}
	}()
	result = &protocol.Schema{Complete: true, Engine: "ydb", ScopeBindings: bindings,
		Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED}, CommonObjects: common.coverage()}
	for _, model := range schema.ObservedModels() {
		result.Coverage.Kinds = append(result.Coverage.Kinds, &protocol.ModelKindCoverage{Model: model,
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Knowledge is limited to returned subjects and fully listed directories"}})
	}
	if len(selectors) == 0 {
		schemas = []string{""}
	}
	read := &traversal{source: source, columns: columns, database: database, common: common, queries: queries, leaveOut: leaveOut, result: result}
	visited := make(map[string]bool)
	for len(schemas) != 0 {
		selected := schemas[0]
		schemas = schemas[1:]
		if visited[selected] {
			return nil, fmt.Errorf("duplicate YDB schema directory")
		}
		if len(visited) >= 4096 {
			return nil, fmt.Errorf("YDB schema traversal exceeds directory limit")
		}
		visited[selected] = true
		directories, err := read.directory(ctx, selected)
		if err != nil {
			return nil, err
		}
		if len(selectors) == 0 {
			schemas = append(schemas, directories...)
		}
	}
	if len(selectors) == 0 && slices.Contains(provider.HostFeatures(ctx), provider.SchemaSubtreeCoverageFeature) {
		common.subtreeRegions(result, database)
	}
	if err := read.streamingInventory(ctx, selectors); err != nil {
		return nil, err
	}
	if len(selectors) == 0 {
		read.nestedRegions(slices.Contains(provider.HostFeatures(ctx), provider.SchemaSubtreeCoverageFeature))
	}
	if err := markTransferReaders(result); err != nil {
		return nil, err
	}
	if err := read.unreadTables(ctx, selectors); err != nil {
		return nil, err
	}
	slices.SortFunc(result.Tables, func(a, b *protocol.Table) int {
		if compared := strings.Compare(a.Identity.Schema, b.Identity.Schema); compared != 0 {
			return compared
		}
		return strings.Compare(a.Identity.Name, b.Identity.Name)
	})
	slices.SortFunc(result.NativeObjects, func(a, b *protocol.Envelope) int {
		if order := strings.Compare(a.Owner.Schema, b.Owner.Schema); order != 0 {
			return order
		}
		return strings.Compare(a.Owner.Name, b.Owner.Name)
	})
	common.sort(result.CommonObjects)
	if err := provider.ValidateSchema(result); err != nil {
		return nil, err
	}
	return result, nil
}

// traversal is one ReadObjects read: what it reads with, the schema it fills,
// and whether it left out a directory at the root that leaveOut names.
type traversal struct {
	source       *source
	columns      ColumnTables
	database     string
	common       Common
	queries      StreamingQuery
	leaveOut     []string
	result       *protocol.Schema
	skippedRealm bool
	// replicas are the replica tables the read leaves out of its tables, and
	// replicationsUnread reports a replication it left uninspected, whose
	// replicas it cannot name.
	replicas           []*protocol.Reference
	replicationsUnread bool
	// unmodeled are the objects of a kind Ptah does not model the read
	// leaves out, reported as unread tables where the host takes them.
	unmodeled []*protocol.UnreadTable
	// nested are the roots of the nested databases the walk left out,
	// relative to database; see leftOut.
	nested []string
}

// directory reads every entry of the listed directory selected into the
// schema, in name order, and returns the subdirectories a traversal of the
// whole database descends into. A directory at the root that leaveOut names
// is neither read nor returned. Once every entry is read, the directory's
// native objects are known completely. A selected path that an object holds is
// a directory with no entries.
//
// A directory whose name starts with a dot holds nothing the read describes,
// as the host's embedded engine read one: it walked past every such
// directory, .sys and .metadata among them, and so planned the creation of
// an object a declaration places there and the drop of none it holds. A
// selected path below one is such a directory too. The walk neither lists
// nor returns one, and the root's claim covers it (see dotted).
//
// A selected path that is the root of a nested database, or lies below one,
// holds nothing of this database either; see nestedDirectory.
func (t *traversal) directory(ctx context.Context, selected string) ([]string, error) {
	if dotted(selected) {
		t.completeDirectory(selected)
		return nil, nil
	}
	listed, err := nativeapi.ListDirectory(ctx, t.source.directory, t.database, selected)
	if nested, isNested := errors.AsType[*nativeapi.NestedDatabaseError](err); isNested {
		t.nestedDirectory(selected, nested.Root)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if listed.GetSelf().GetName() != path.Base(path.Join(t.database, selected)) {
		return nil, fmt.Errorf("YDB schema selector did not describe the expected directory")
	}
	// An object at the selected path, such as a table a plan replaces with a
	// directory of secrets, leaves no directory there: YDB needs every path
	// above an object to be a directory, so nothing is below it. The read of
	// the object belongs to the directory that lists it.
	if selected != "" && occupant(listed.GetSelf().GetType()) {
		t.completeDirectory(selected)
		return nil, nil
	}
	if listed.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY && (selected != "" || listed.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE) {
		return nil, fmt.Errorf("YDB schema selector did not describe the expected directory")
	}
	children := slices.Clone(listed.GetChildren())
	slices.SortFunc(children, func(a, b *Ydb_Scheme.Entry) int { return strings.Compare(a.GetName(), b.GetName()) })
	names := make(map[string]bool)
	var directories []string
	for _, entry := range children {
		name := entry.GetName()
		if !component(name) || names[name] {
			return nil, fmt.Errorf("invalid or duplicate YDB directory entry")
		}
		names[name] = true
		if t.leftOut(selected, entry) {
			continue
		}
		if entry.GetType() == Ydb_Scheme.Entry_DIRECTORY {
			directories = append(directories, path.Join(selected, name))
		} else if err := t.entry(ctx, selected, name, entry.GetType()); err != nil {
			return nil, err
		}
		if proto.Size(t.result) > 3<<20 {
			return nil, fmt.Errorf("YDB schema exceeds snapshot size limit")
		}
		if len(t.result.Tables)+len(t.result.NativeObjects)+len(t.result.ReservedObjects) > 4096 {
			return nil, fmt.Errorf("YDB schema read exceeds object limit")
		}
	}
	t.completeDirectory(selected)
	return directories, nil
}

// completeDirectory records that the read knows every native object and every
// table facet of the directory selected. A successful list and complete child
// reads prove native-object absence only in this exact directory, never in
// skipped descendants.
//
// The read describes every table of the directory with each facet its kind of
// table has, a row table's settings, indexes, changefeeds and the settings of
// its global indexes, a column table's storage and every column's properties,
// so the facet kinds are known completely there too. A host that did not
// negotiate schema.observed-index-facets has no place for an index's settings,
// so none of them is missing either. Without that the kinds stay narrowed, and
// an export the read knows completely could not be written: a source has no
// limit that says a facet kind.
func (t *traversal) completeDirectory(selected string) {
	for _, record := range t.result.Coverage.Kinds {
		if slices.Contains(tablecompare.Kinds(), record.Model.Kind) {
			record.Regions = append(record.Regions, directoryRegion(t.database, selected))
			continue
		}
		// Views the common wire carries are not read as native objects.
		if schema.IsNativeObject(record.Model.Kind) && (record.Model.Kind != schema.ViewProperties || !t.common.Views) {
			record.Regions = append(record.Regions, directoryRegion(t.database, selected))
		}
	}
}

// nestedDirectory records the selected directory, the root of the nested
// database at root or a directory below it, as holding nothing of this
// database. The host's embedded engine read such a selection as an empty
// directory: its walk never entered a nested database. Read as a directory to
// list, the selection is refused (`YDB schema selector did not describe the
// expected directory` at the root, `its parent is not a directory` below it),
// and so is every read, introspection and apply that names it.
//
// The directory is claimed complete, as a listed one is (see
// completeDirectory), and each claim carries the nested database's reason: a
// session of this database neither reads nor drops what the nested database
// holds, and an object it creates there lands in the nested database's scheme.
// The comparison and the planning check read the reason back and refuse a
// declaration there (see objectplan.NestedDatabaseCreations), and the access
// read leaves the directory out. A claim of unknown knowledge would make a Go
// export declare every native kind not described.
func (t *traversal) nestedDirectory(selected, root string) {
	t.nested = append(t.nested, root)
	scope := directoryRegion(t.database, selected).GetScope()
	t.nestedClaims(scope, root, func(kind string) bool { return slices.Contains(tablecompare.Kinds(), kind) || t.nestedKind(kind) })
}

// nestedRegions claims the root of each nested database a read of the whole
// database left out, and with subtree every directory below it, for the
// native kinds, as nestedDirectory claims a selected one. Like the root's
// claim, each is complete: this database holds nothing there. The table facets
// get no claim, as no directory the walk did not list gets one.
func (t *traversal) nestedRegions(subtree bool) {
	for _, root := range t.nested {
		scope := directoryRegion(t.database, root).GetScope()
		scope.SchemaSubtree = subtree
		t.nestedClaims(scope, root, t.nestedKind)
	}
}

// nestedClaims claims each kind claimed selects complete at scope, with the
// reason of the nested database at root.
func (t *traversal) nestedClaims(scope *protocol.ModelNamespace, root string, claimed func(string) bool) {
	for _, record := range t.result.Coverage.Kinds {
		if claimed(record.Model.Kind) {
			record.Regions = append(record.Regions, &protocol.ModelNamespaceKnowledge{Scope: proto.CloneOf(scope),
				Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE, Reason: schema.NestedDatabaseReason(root)}})
		}
	}
}

// nestedKind reports whether the read gives its claim on kind at a nested
// database the nested database's reason: every native kind but views the
// common wire carries, which are not read as native objects.
func (t *traversal) nestedKind(kind string) bool {
	return schema.IsNativeObject(kind) && (kind != schema.ViewProperties || !t.common.Views)
}

// occupant reports whether an entry of kind is an object entry reads: one
// that holds its path, so that no directory can be there.
func occupant(kind Ydb_Scheme.Entry_Type) bool {
	switch kind {
	case streamingQueryEntry, Ydb_Scheme.Entry_TABLE, Ydb_Scheme.Entry_COLUMN_TABLE, Ydb_Scheme.Entry_TOPIC, Ydb_Scheme.Entry_PERS_QUEUE_GROUP,
		Ydb_Scheme.Entry_REPLICATION, Ydb_Scheme.Entry_TRANSFER, Ydb_Scheme.Entry_COORDINATION_NODE, Ydb_Scheme.Entry_SECRET, Ydb_Scheme.Entry_VIEW,
		Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE, Ydb_Scheme.Entry_EXTERNAL_TABLE:
		return true
	}
	return false
}

// entry reads the listed entry name of the directory selected into the schema.
// The entry is anything but a directory: an object it reads, one it reserves,
// or one it leaves to the object that owns it.
func (t *traversal) entry(ctx context.Context, selected, name string, kind Ydb_Scheme.Entry_Type) error {
	switch kind {
	case streamingQueryEntry:
		return t.readStreamingQuery(ctx, selected, name)
	case Ydb_Scheme.Entry_TABLE:
		return t.readRowTable(ctx, selected, name)
	case Ydb_Scheme.Entry_COLUMN_TABLE:
		table, key, err := t.source.readColumnTable(ctx, t.columns, t.database, selected, name)
		if err != nil {
			return err
		}
		t.common.primaryKey(table, key)
		t.addColumnTable(table)
	case Ydb_Scheme.Entry_TOPIC:
		return t.readTopic(ctx, selected, name)
	case Ydb_Scheme.Entry_PERS_QUEUE_GROUP:
		// A queue of the older persistent queue kind is recorded, not
		// read, as the host's embedded engine recorded it: no statement
		// Ptah writes creates one, and read as a topic it would be
		// altered or dropped as one. A plan neither keeps nor drops it.
		t.unread(&protocol.Reference{Engine: "ydb", Database: t.database, Schema: selected, Kind: schema.TopicProperties, Name: name},
			protocol.ModelKnowledge_UNINSPECTED, queueGroupReason)
	case Ydb_Scheme.Entry_REPLICATION, Ydb_Scheme.Entry_TRANSFER:
		return t.readReplication(ctx, selected, name, kind)
	case Ydb_Scheme.Entry_COORDINATION_NODE:
		if selected == "" && name == locking.NodeName {
			// The lock service takes the node at the root of the
			// session's database by its name, so the name is the
			// reservation. Read as a managed node, it would be
			// exported into a source that declarations then refuse.
			t.common.reserve(t.result, &protocol.Reference{Engine: "ydb", Database: t.database, Kind: schema.CoordinationProperties, Name: name}, protocol.ReservedObject_LOCK)
			return nil
		}
		fallthrough
	case Ydb_Scheme.Entry_SECRET, Ydb_Scheme.Entry_VIEW, Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE, Ydb_Scheme.Entry_EXTERNAL_TABLE:
		return t.readDescribed(ctx, selected, name, kind)
	case Ydb_Scheme.Entry_COLUMN_STORE, Ydb_Scheme.Entry_SEQUENCE:
		// An object Ptah does not model is recorded and the read goes
		// on, as the host's embedded engine recorded it as not described.
		// It holds its path, so a table declared there could not be
		// created: a host that takes unread tables is told so.
		t.unmodeled = append(t.unmodeled, &protocol.UnreadTable{Table: &protocol.Reference{Engine: "ydb", Database: t.database, Schema: selected, Kind: "table", Name: name},
			Diagnostic: &protocol.ProviderDiagnostic{Reason: unmodeledReason(selected, name, kind)}})
	case Ydb_Scheme.Entry_SYS_VIEW:
		// A system view outside a dot-directory belongs to the server
		// too, as the host's embedded engine left it out.
	default:
		return fmt.Errorf("YDB object %s is a %s, which Ptah does not read; remove it from the database or read a schema that does not hold it",
			path.Join(t.database, selected, name), entryTypeName(kind))
	}
	return nil
}

// queueGroupReason is why a read records a queue of the older persistent
// queue kind rather than reading it, in the host's words.
const queueGroupReason = "a persistent queue group, which Ptah does not read"

// unread records the object ref as one the read lists and does not describe,
// with knowledge in state for reason.
func (t *traversal) unread(ref *protocol.Reference, state protocol.ModelKnowledge_State, reason string) {
	t.result.Coverage.Subjects = append(t.result.Coverage.Subjects, &protocol.ModelSubjectCoverage{
		Kind: ref.Kind, Subject: proto.CloneOf(ref), NormalizedSubject: proto.CloneOf(ref),
		Knowledge: &protocol.ModelKnowledge{State: state, Reason: reason},
	})
}

// unmodeledReason says why a read leaves out the object name of the directory
// selected, an entry of a kind Ptah does not model.
func unmodeledReason(selected, name string, kind Ydb_Scheme.Entry_Type) string {
	if selected != "" {
		name = selected + "." + name
	}
	what := "a column store"
	if kind == Ydb_Scheme.Entry_SEQUENCE {
		what = "a sequence"
	}
	reason := fmt.Sprintf("%s is %s, which Ptah does not read", name, what)
	if len(reason) > maxReasonBytes {
		reason = fmt.Sprintf("it is %s, which Ptah does not read", what)
	}
	return reason
}

// entryTypeName names a scheme entry type, including one the pinned protocol
// buffers do not know.
func entryTypeName(kind Ydb_Scheme.Entry_Type) string {
	if name, known := Ydb_Scheme.Entry_Type_name[int32(kind)]; known {
		return name
	}
	return fmt.Sprintf("scheme entry of type %d", int32(kind))
}

// readStreamingQuery reads the stored settings of a listed streaming query. A
// query the read cannot establish settings for is recorded as uninspected.
func (t *traversal) readStreamingQuery(ctx context.Context, selected, name string) error {
	owner := &protocol.Reference{Engine: "ydb", Database: t.database, Schema: selected, Kind: schema.StreamingQueryProperties, Name: name}
	var spec *streaming.Spec
	readErr := fmt.Errorf("the streaming query capability was not established")
	if t.queries != nil {
		spec, readErr = t.queries(ctx, path.Join(t.database, selected, name))
		if readErr == nil && spec == nil {
			readErr = fmt.Errorf("the listed streaming query has no stored settings")
		}
	}
	if readErr != nil {
		t.result.Coverage.Subjects = append(t.result.Coverage.Subjects, &protocol.ModelSubjectCoverage{
			Kind: owner.Kind, Subject: proto.CloneOf(owner), NormalizedSubject: proto.CloneOf(owner),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: readErr.Error()},
		})
		return nil
	}
	value, err := schema.CaptureObject(owner, &nativeprotocol.StreamingQuerySettings{Text: spec.Text, Run: spec.Run, ResourcePool: spec.ResourcePool})
	if err != nil {
		return err
	}
	t.addObject(value)
	return nil
}

// readRowTable reads a listed row table with its indexes and changefeeds. A
// reserved table is recorded as reserved, and a replica is left to the
// replication that writes it.
func (t *traversal) readRowTable(ctx context.Context, selected, name string) error {
	description, err := t.source.describeListedTable(ctx, path.Join(t.database, selected), name)
	if err != nil {
		return err
	}
	if description.reservation != protocol.ReservedObject_PURPOSE_UNSPECIFIED {
		t.common.reserve(t.result, &protocol.Reference{Engine: "ydb", Database: t.database, Schema: selected, Kind: "table", Name: name}, description.reservation)
		return nil
	}
	// A table an async replication writes, or wrote without
	// failing over, is the replication's: YDB refuses every
	// write and ALTER on it, and a plan that read it as an
	// ordinary table would drop it as undeclared or alter it.
	// The replication's items name it; the read leaves it out
	// of the tables, as the host's embedded engine does.
	if replication.IsReplica(description.table.GetAttributes()) {
		t.replicas = append(t.replicas, &protocol.Reference{Engine: "ydb", Database: t.database, Schema: selected, Kind: "table", Name: name})
		return nil
	}
	// A family that holds what the model does not know, from a newer
	// server, leaves the table's properties unrepresentable rather than the
	// read failed: the table is read without them, and a plan that would
	// change or rebuild them stops (see tablecompare.Compare and
	// tablecreate's rebuild).
	described, unknown := schema.ReadableFamilies(description.table)
	table, err := readTable(t.database, selected, name, described)
	if err != nil {
		return err
	}
	if unknown != "" {
		table.Facets = slices.DeleteFunc(table.Facets, func(facet *protocol.Envelope) bool { return facet.GetKind() == schema.TableProperties })
		t.result.Coverage.Subjects = append(t.result.Coverage.Subjects, &protocol.ModelSubjectCoverage{
			Kind: schema.TableProperties, Subject: proto.CloneOf(table.Identity), NormalizedSubject: proto.CloneOf(table.Identity),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNREPRESENTABLE, Reason: "table " + path.Join(selected, name) + ": " + unknown},
		})
	}
	t.common.primaryKey(table, description.table.GetPrimaryKey())
	t.common.tableComments(t.result.CommonObjects, table.Identity, description)
	indexes, err := t.source.readIndexes(ctx, path.Join(t.database, selected, name), description)
	if err != nil {
		return err
	}
	if t.common.Indexes {
		// The host holds what the common model describes; the
		// native facet keeps only the rest.
		table.Indexes, err = t.common.commonIndexes(table.Identity, indexes)
		if err != nil {
			return err
		}
		indexes = nativeIndexes(indexes)
	}
	facet, err := schema.CaptureIndexes(table.Identity, indexes)
	if err != nil {
		return err
	}
	table.Facets = append(table.Facets, facet)
	feeds, err := t.source.readChangefeeds(ctx, path.Join(t.database, selected, name), description)
	if err != nil {
		return err
	}
	feedFacet, err := schema.CaptureChangefeeds(table.Identity, feeds)
	if err != nil {
		return err
	}
	table.Facets = append(table.Facets, feedFacet)
	t.addTable(table)
	return nil
}

// readTopic reads a listed topic as a native object.
func (t *traversal) readTopic(ctx context.Context, selected, name string) error {
	topic, err := t.source.describeTopic(ctx, path.Join(t.database, selected, name))
	if err != nil {
		return err
	}
	if err := topicIdentity(topic, name); err != nil {
		return err
	}
	owner := &protocol.Reference{Engine: "ydb", Database: t.database, Schema: selected, Kind: schema.TopicProperties, Name: name}
	value, err := schema.CaptureObject(owner, topic)
	if err != nil {
		return err
	}
	t.addObject(value)
	return nil
}

// readReplication reads a listed async replication or transfer as a native
// object. One the read cannot describe is recorded as uninspected.
func (t *traversal) readReplication(ctx context.Context, selected, name string, entryType Ydb_Scheme.Entry_Type) error {
	kind := schema.ReplicationProperties
	if entryType == Ydb_Scheme.Entry_TRANSFER {
		kind = schema.TransferProperties
	}
	owner := &protocol.Reference{Engine: "ydb", Database: t.database, Schema: selected, Kind: kind, Name: name}
	value, err := t.source.replications.Capture(ctx, t.database, owner)
	if reason := unreadReplication(err); reason != "" {
		t.result.Coverage.Subjects = append(t.result.Coverage.Subjects, &protocol.ModelSubjectCoverage{Kind: kind, Subject: proto.CloneOf(owner), NormalizedSubject: proto.CloneOf(owner),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: reason}})
		t.replicationsUnread = t.replicationsUnread || kind == schema.ReplicationProperties
		return nil
	}
	if err != nil {
		return err
	}
	t.addObject(value)
	return nil
}

// readDescribed reads a listed secret, view, external data source, external
// table or coordination node. A view goes to the common objects when the read
// takes common views, and anything else is a native object. A coordination
// node whose description holds what the provider does not model is recorded
// as unrepresentable, and the read goes on.
func (t *traversal) readDescribed(ctx context.Context, selected, name string, entryType Ydb_Scheme.Entry_Type) error {
	kind, described, err := t.source.readObject(ctx, path.Join(t.database, selected, name), entryType)
	if err != nil {
		return err
	}
	if view, isView := described.(*nativeprotocol.ViewObservation); isView && t.common.Views {
		t.common.view(t.result.CommonObjects, &protocol.Reference{Engine: "ydb", Database: t.database, Schema: selected, Kind: "view", Name: name}, view)
		return nil
	}
	owner := &protocol.Reference{Engine: "ydb", Database: t.database, Schema: selected, Kind: kind, Name: name}
	if node, isNode := described.(*Ydb_Coordination.DescribeNodeResult); isNode {
		if reason := unrepresentableNode(node); reason != "" {
			t.unread(owner, protocol.ModelKnowledge_UNREPRESENTABLE, fmt.Sprintf("YDB coordination node %s: %s", path.Join(t.database, selected, name), reason))
			return nil
		}
	}
	value, err := schema.CaptureObject(owner, described)
	if err != nil {
		return err
	}
	t.addObject(value)
	return nil
}

// addTable adds a row table to the schema, with the coverage of its facets,
// of its columns' facets and of its indexes' facets.
func (t *traversal) addTable(table *protocol.Table) {
	t.result.Tables = append(t.result.Tables, table)
	observe(t.result, table.Facets)
	for _, column := range table.Columns {
		observe(t.result, column.Facets)
	}
	for _, index := range table.Indexes {
		observe(t.result, index.Facets)
	}
}

// addColumnTable adds a column table to the schema, with the coverage of its
// facets, the absence of row-table properties, and the coverage of its
// columns' facets.
func (t *traversal) addColumnTable(table *protocol.Table) {
	t.result.Tables = append(t.result.Tables, table)
	observe(t.result, table.Facets)
	absentRowProperties(t.result, table.Facets)
	for _, column := range table.Columns {
		observe(t.result, column.Facets)
	}
}

// unreadTables names, when the read is asked for them, the objects of a kind
// Ptah does not model the read left out, and the replica tables it left out
// that no replication writes: against the targets of the replications the
// snapshot holds for a read of the whole database, and of every replication
// of the database for a selected read.
func (t *traversal) unreadTables(ctx context.Context, selectors []string) error {
	if !t.common.UnreadTables {
		return nil
	}
	defer func() { t.result.UnreadTables = append(t.result.UnreadTables, t.unmodeled...) }()
	if len(t.replicas) == 0 {
		return nil
	}
	targets, complete := []string(nil), !t.replicationsUnread
	var err error
	if len(selectors) == 0 {
		targets, err = snapshotTargets(t.result)
	} else {
		targets, complete, err = t.source.databaseTargets(ctx, t.database, t.leaveOut)
	}
	if err != nil {
		return err
	}
	t.result.UnreadTables = unreadReplicas(t.replicas, replicationTargets{paths: targets, complete: complete})
	return nil
}

// addObject adds a native object to the schema, with its coverage.
func (t *traversal) addObject(value *protocol.Envelope) {
	t.result.NativeObjects = append(t.result.NativeObjects, value)
	observe(t.result, []*protocol.Envelope{value})
}

// streamingInventory replaces the streaming coverage of a read of the whole
// database with the inventory of every directory, and gives a selected read
// the queries outside its selection as planning context when common asks
// for it.
//
// Complete streaming dependency assessment needs queries in every user
// directory, including dot-prefixed paths. Explicit selectors retain the
// narrower coverage the traversal captured: a filtered read may claim
// nothing outside them. A read that left out dev realms takes the inventory unless
// a query lives in one of them; see RealmStreamingInventory.
func (t *traversal) streamingInventory(ctx context.Context, selectors []string) error {
	if len(selectors) == 0 {
		inventory, err := ReadStreamingInventory(ctx, t.database, t.source.directory, t.queries)
		switch {
		case err != nil && (!t.skippedRealm || ctx.Err() != nil):
			return err
		case err == nil && !t.skippedRealm:
			replaceStreamingInventory(t.result, inventory)
		case err == nil:
			RealmStreamingInventory(t.result, inventory, t.leaveOut)
		}
	} else if t.common.ContextInventory {
		inventory, err := ReadStreamingInventory(ctx, t.database, t.source.directory, t.queries)
		if err != nil && ctx.Err() != nil {
			return err
		}
		if err == nil {
			t.result.PlanningContext = OutsideStreamingInventory(inventory, selectors)
		}
	}
	return nil
}

// leftOut reports a directory entry of the listed directory selected that the
// walk neither reads nor returns: a nested database, a directory whose name
// starts with a dot (see dotted), and a directory at the root that leaveOut
// names. The dev realms other runs keep there are no part of the database's
// schema, as the host's embedded engine left them out, so the root's claim
// covers that directory as holding nothing; the walk records that it left the
// realms out.
//
// A nested database is another database whose root sits below this one, such
// as /local/db2 below /local. YDB lists it as an entry of type DATABASE, and
// what it holds is that database's schema, not a directory of this one: a
// session of this database cannot drop a table there (`Cannot find table`).
// The host's embedded engine left every such entry out; a read that refused
// it refused the whole database. The walk records its root for nestedRegions.
func (t *traversal) leftOut(selected string, entry *Ydb_Scheme.Entry) bool {
	if entry.GetType() == Ydb_Scheme.Entry_DATABASE {
		t.nested = append(t.nested, path.Join(selected, entry.GetName()))
		return true
	}
	if entry.GetType() != Ydb_Scheme.Entry_DIRECTORY {
		return false
	}
	if dotted(entry.GetName()) {
		return true
	}
	if selected == "" && slices.Contains(t.leaveOut, entry.GetName()) {
		t.skippedRealm = true
		return true
	}
	return false
}

// dotted reports a directory path one of whose names starts with a dot. The
// walk describes nothing below such a name, as the host's embedded engine
// described nothing there, and claims the path as an empty directory; only
// the streaming inventory, which runs on its own, looks inside. Reading one
// would also plan the drop of each object there that no declaration names,
// which that engine never planned.
func dotted(directory string) bool {
	return slices.ContainsFunc(strings.Split(directory, "/"), func(name string) bool { return strings.HasPrefix(name, ".") })
}

func component(value string) bool {
	return utf8.ValidString(value) && value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

func scopes(database string, selectors, leaveOut []string) ([]string, []*protocol.InspectionScopeBinding, error) {
	var directories []string
	var bindings []*protocol.InspectionScopeBinding
	seen := make(map[string]bool)
	for _, selector := range selectors {
		if strings.TrimSpace(selector) == "" {
			return nil, nil, fmt.Errorf("use / to select the YDB database root")
		}
		selected := strings.Trim(selector, "/")
		if selected != "" {
			for part := range strings.SplitSeq(selected, "/") {
				if !component(part) {
					return nil, nil, fmt.Errorf("invalid YDB schema selector")
				}
			}
			if slices.Contains(leaveOut, strings.Split(selected, "/")[0]) {
				return nil, nil, fmt.Errorf("YDB schema selector names a directory that belongs to other runs")
			}
		}
		if seen[selected] {
			return nil, nil, fmt.Errorf("YDB schema selectors overlap")
		}
		seen[selected] = true
		directories = append(directories, selected)
		bindings = append(bindings, &protocol.InspectionScopeBinding{Selector: selector, Namespaces: []*protocol.ModelNamespace{{
			Database: &protocol.NamespaceComponent{Source: database, Normalized: database},
			Schema:   &protocol.NamespaceComponent{Source: selected, Normalized: selected},
		}}})
	}
	return directories, bindings, nil
}

func readTable(database, directory, name string, described *nativeprotocol.DescribeTableResult) (*protocol.Table, error) {
	if described.GetSelf().GetName() != name || described.GetSelf().GetType() != Ydb_Scheme.Entry_TABLE {
		return nil, fmt.Errorf("YDB listing and table description disagree")
	}
	ref := &protocol.Reference{Engine: "ydb", Database: database, Schema: directory, Kind: "table", Name: name}
	table := &protocol.Table{Identity: ref, NormalizedIdentity: proto.CloneOf(ref)}
	facet, err := schema.CaptureTable(ref, described)
	if err != nil {
		return nil, err
	}
	table.Facets = []*protocol.Envelope{facet}
	names := make(map[string]bool)
	for position, column := range described.GetColumns() {
		if column.GetName() == "" || names[column.GetName()] {
			return nil, fmt.Errorf("invalid YDB column identity")
		}
		names[column.GetName()] = true
		observed, err := columnsemantics.Observed(column, position+1, slices.Contains(described.GetPrimaryKey(), column.GetName()))
		if err != nil {
			return nil, err
		}
		projected := &protocol.Column{Name: column.GetName(), Observed: observed}
		facet, err := schema.CaptureColumn(provider.ColumnReference(ref, column.GetName()), column)
		if err != nil {
			return nil, err
		}
		projected.Facets = []*protocol.Envelope{facet}
		table.Columns = append(table.Columns, projected)
	}
	keys := make(map[string]bool)
	for _, key := range described.GetPrimaryKey() {
		if !names[key] || keys[key] {
			return nil, fmt.Errorf("invalid YDB primary key description")
		}
		keys[key] = true
	}
	if len(keys) == 0 || len(table.Columns) == 0 {
		return nil, fmt.Errorf("incomplete YDB row table description")
	}
	return table, nil
}

func observe(snapshot *protocol.Schema, values []*protocol.Envelope) {
	for _, value := range values {
		snapshot.Coverage.Subjects = append(snapshot.Coverage.Subjects, &protocol.ModelSubjectCoverage{
			Kind: value.Kind, Subject: proto.CloneOf(value.Owner), NormalizedSubject: proto.CloneOf(value.Owner),
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
		})
	}
}

// absentRowProperties records that a column table holds no row-table
// properties: a declaration that states a TTL alone, as an HCL platform block
// does, is compared with such a table as its column storage's TTL, and the
// change answers the row-table properties the declaration states (see
// tablecompare.Compare).
func absentRowProperties(snapshot *protocol.Schema, facets []*protocol.Envelope) {
	for _, facet := range facets {
		if facet.GetKind() == schema.ColumnStorageProperties {
			snapshot.Coverage.Subjects = append(snapshot.Coverage.Subjects, &protocol.ModelSubjectCoverage{
				Kind: schema.TableProperties, Subject: proto.CloneOf(facet.GetOwner()), NormalizedSubject: proto.CloneOf(facet.GetOwner()),
				Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_ABSENT},
			})
		}
	}
}
