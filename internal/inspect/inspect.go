package inspect

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// The pinned vendor protobuf does not name STREAMING_QUERY.
const streamingQueryEntry = Ydb_Scheme.Entry_Type(26)

// ColumnTables reads the monitoring endpoint's description of the column table
// at an absolute path: the page columnstore.Decode reads.
type ColumnTables func(ctx context.Context, absolute string) ([]byte, error)

// Read captures exact common column fields and native property facets. A
// partial traversal, unmodeled object, or incomplete native description returns
// no snapshot. Filtered schemas are exact directories, not recursive prefixes.
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
	visited := map[string]bool{}
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
		listed, err := source.directory(ctx, path.Join(database, selected))
		if err != nil {
			return nil, err
		}
		if listed.GetSelf().GetName() != path.Base(path.Join(database, selected)) ||
			(listed.GetSelf().GetType() != Ydb_Scheme.Entry_DIRECTORY && (selected != "" || listed.GetSelf().GetType() != Ydb_Scheme.Entry_DATABASE)) {
			return nil, fmt.Errorf("YDB schema selector did not describe the expected directory")
		}
		children := slices.Clone(listed.GetChildren())
		slices.SortFunc(children, func(a, b *Ydb_Scheme.Entry) int { return strings.Compare(a.GetName(), b.GetName()) })
		names := map[string]bool{}
		for _, entry := range children {
			name := entry.GetName()
			if !component(name) || names[name] {
				return nil, fmt.Errorf("invalid or duplicate YDB directory entry")
			}
			names[name] = true
			if strings.HasPrefix(name, ".") && entry.GetType() == Ydb_Scheme.Entry_DIRECTORY {
				continue
			}
			if selected == "" && entry.GetType() == Ydb_Scheme.Entry_DIRECTORY && slices.Contains(leaveOut, name) {
				continue
			}
			switch entry.GetType() {
			case streamingQueryEntry:
				owner := &protocol.Reference{Engine: "ydb", Database: database, Schema: selected, Kind: schema.StreamingQueryProperties, Name: name}
				var spec *streaming.Spec
				readErr := fmt.Errorf("the streaming query capability was not established")
				if queries != nil {
					spec, readErr = queries(ctx, path.Join(database, selected, name))
					if readErr == nil && spec == nil {
						readErr = fmt.Errorf("the listed streaming query has no stored settings")
					}
				}
				if readErr != nil {
					result.Coverage.Subjects = append(result.Coverage.Subjects, &protocol.ModelSubjectCoverage{Kind: owner.Kind, Subject: proto.CloneOf(owner), NormalizedSubject: proto.CloneOf(owner), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: readErr.Error()}})
					break
				}
				value, err := schema.CaptureObject(owner, &nativeprotocol.StreamingQuerySettings{Text: spec.Text, Run: spec.Run, ResourcePool: spec.ResourcePool})
				if err != nil {
					return nil, err
				}
				result.NativeObjects = append(result.NativeObjects, value)
				observe(result, []*protocol.Envelope{value})
			case Ydb_Scheme.Entry_DIRECTORY:
				if len(selectors) == 0 {
					schemas = append(schemas, path.Join(selected, name))
				}
			case Ydb_Scheme.Entry_TABLE:
				description, err := source.describeTable(ctx, path.Join(database, selected, name))
				if err != nil {
					return nil, err
				}
				if description.reservation != protocol.ReservedObject_PURPOSE_UNSPECIFIED {
					reserve(result, &protocol.Reference{Engine: "ydb", Database: database, Schema: selected, Kind: "table", Name: name}, description.reservation)
					break
				}
				table, err := readTable(database, selected, name, description.table)
				if err != nil {
					return nil, err
				}
				common.tableComments(result.CommonObjects, table.Identity, description)
				indexes, err := source.readIndexes(ctx, path.Join(database, selected, name), description)
				if err != nil {
					return nil, err
				}
				facet, err := schema.CaptureIndexes(table.Identity, indexes)
				if err != nil {
					return nil, err
				}
				table.Facets = append(table.Facets, facet)
				feeds, err := source.readChangefeeds(ctx, path.Join(database, selected, name), description)
				if err != nil {
					return nil, err
				}
				feedFacet, err := schema.CaptureChangefeeds(table.Identity, feeds)
				if err != nil {
					return nil, err
				}
				table.Facets = append(table.Facets, feedFacet)
				result.Tables = append(result.Tables, table)
				observe(result, table.Facets)
				for _, column := range table.Columns {
					observe(result, column.Facets)
				}
			case Ydb_Scheme.Entry_COLUMN_TABLE:
				table, err := source.readColumnTable(ctx, columns, database, selected, name)
				if err != nil {
					return nil, err
				}
				result.Tables = append(result.Tables, table)
				observe(result, table.Facets)
				for _, column := range table.Columns {
					observe(result, column.Facets)
				}
			case Ydb_Scheme.Entry_TOPIC, Ydb_Scheme.Entry_PERS_QUEUE_GROUP:
				topic, err := source.describeTopic(ctx, path.Join(database, selected, name))
				if err != nil {
					return nil, err
				}
				if err := topicIdentity(topic, name); err != nil {
					return nil, err
				}
				owner := &protocol.Reference{Engine: "ydb", Database: database, Schema: selected, Kind: schema.TopicProperties, Name: name}
				value, err := schema.CaptureObject(owner, topic)
				if err != nil {
					return nil, err
				}
				result.NativeObjects = append(result.NativeObjects, value)
				observe(result, []*protocol.Envelope{value})
			case Ydb_Scheme.Entry_VIEW, Ydb_Scheme.Entry_COORDINATION_NODE, Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE, Ydb_Scheme.Entry_EXTERNAL_TABLE:
				kind, described, err := source.readObject(ctx, path.Join(database, selected, name), entry.GetType())
				if err != nil {
					return nil, err
				}
				if view, isView := described.(*nativeprotocol.ViewObservation); isView && common.Views {
					common.view(result.CommonObjects, &protocol.Reference{Engine: "ydb", Database: database, Schema: selected, Kind: "view", Name: name}, view)
					break
				}
				owner := &protocol.Reference{Engine: "ydb", Database: database, Schema: selected, Kind: kind, Name: name}
				value, err := schema.CaptureObject(owner, described)
				if err != nil {
					return nil, err
				}
				result.NativeObjects = append(result.NativeObjects, value)
				observe(result, []*protocol.Envelope{value})
			default:
				return nil, fmt.Errorf("YDB object kind %s needs an extracted reader", entry.GetType())
			}
			if proto.Size(result) > 3<<20 {
				return nil, fmt.Errorf("YDB schema exceeds snapshot size limit")
			}
			if len(result.Tables)+len(result.NativeObjects)+len(result.ReservedObjects) > 4096 {
				return nil, fmt.Errorf("YDB schema read exceeds object limit")
			}
		}
		// A successful list and complete child reads prove native-object
		// absence only in this exact directory, never in skipped descendants.
		for _, record := range result.Coverage.Kinds {
			if record.Model.Kind == schema.StreamingQueryProperties && queries == nil {
				continue
			}
			// Views the common wire carries are not read as native objects.
			if schema.IsNativeObject(record.Model.Kind) && (record.Model.Kind != schema.ViewProperties || !common.Views) {
				record.Regions = append(record.Regions, &protocol.ModelNamespaceKnowledge{
					Scope:     &protocol.ModelNamespace{Database: &protocol.NamespaceComponent{Source: database, Normalized: database}, Catalog: &protocol.NamespaceComponent{}, Schema: &protocol.NamespaceComponent{Source: selected, Normalized: selected}},
					Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
				})
			}
		}
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

func component(value string) bool {
	return utf8.ValidString(value) && value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

func scopes(database string, selectors, leaveOut []string) ([]string, []*protocol.InspectionScopeBinding, error) {
	var directories []string
	var bindings []*protocol.InspectionScopeBinding
	seen := map[string]bool{}
	for _, selector := range selectors {
		if strings.TrimSpace(selector) == "" {
			return nil, nil, fmt.Errorf("use / to select the YDB database root")
		}
		selected := strings.Trim(selector, "/")
		if selected != "" {
			for _, part := range strings.Split(selected, "/") {
				if !component(part) || strings.HasPrefix(part, ".") {
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
	if _, replica := described.GetAttributes()["__async_replica"]; replica {
		return nil, fmt.Errorf("YDB replica table requires its replication owner")
	}
	ref := &protocol.Reference{Engine: "ydb", Database: database, Schema: directory, Kind: "table", Name: name}
	table := &protocol.Table{Identity: ref, NormalizedIdentity: proto.CloneOf(ref)}
	facet, err := schema.CaptureTable(ref, described)
	if err != nil {
		return nil, err
	}
	table.Facets = []*protocol.Envelope{facet}
	names := map[string]bool{}
	for _, column := range described.GetColumns() {
		if column.GetName() == "" || names[column.GetName()] {
			return nil, fmt.Errorf("invalid YDB column identity")
		}
		names[column.GetName()] = true
		spelling, nullable, err := schema.ColumnType(column.GetType())
		if err != nil {
			return nil, err
		}
		if column.GetNotNull() {
			nullable = false
		}
		projected := &protocol.Column{Name: column.GetName(), Type: spelling, Nullable: nullable, PrimaryKey: slices.Contains(described.GetPrimaryKey(), column.GetName())}
		if literal := column.GetFromLiteral(); literal != nil {
			text, err := columnsemantics.Literal(literal)
			if err != nil {
				return nil, fmt.Errorf("column %q default: %w", column.GetName(), err)
			}
			projected.DefaultExpression = new(text)
		}
		facet, err := schema.CaptureColumn(provider.ColumnReference(ref, column.GetName()), column)
		if err != nil {
			return nil, err
		}
		projected.Facets = []*protocol.Envelope{facet}
		table.Columns = append(table.Columns, projected)
	}
	keys := map[string]bool{}
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
