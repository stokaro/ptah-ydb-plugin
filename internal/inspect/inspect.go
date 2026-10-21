package inspect

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/proto"
)

// Read captures exact common column fields and native property facets. A
// partial traversal, unmodeled object, or incomplete native description returns
// no snapshot. Filtered schemas are exact directories, not recursive prefixes.
func Read(ctx context.Context, driver *ydb.Driver, database string, selectors []string) (result *protocol.Schema, err error) {
	schemas, bindings, err := scopes(database, selectors)
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
		Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED}}
	for _, model := range schema.Models() {
		result.Coverage.Kinds = append(result.Coverage.Kinds, &protocol.ModelKindCoverage{Model: model,
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Only returned table and column subjects were inspected"}})
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
			switch entry.GetType() {
			case Ydb_Scheme.Entry_DIRECTORY:
				if len(selectors) == 0 {
					schemas = append(schemas, path.Join(selected, name))
				}
			case Ydb_Scheme.Entry_TABLE:
				description, err := source.describeTable(ctx, path.Join(database, selected, name))
				if err != nil {
					return nil, err
				}
				table, err := readTable(database, selected, name, description.table)
				if err != nil {
					return nil, err
				}
				indexes, err := source.readIndexes(ctx, path.Join(database, selected, name), description)
				if err != nil {
					return nil, err
				}
				facet, err := schema.CaptureIndexes(table.Identity, indexes)
				if err != nil {
					return nil, err
				}
				table.Facets = append(table.Facets, facet)
				result.Tables = append(result.Tables, table)
				observe(result, table.Facets)
				for _, column := range table.Columns {
					observe(result, column.Facets)
				}
			default:
				return nil, fmt.Errorf("YDB object kind %s needs an extracted reader", entry.GetType())
			}
			if proto.Size(result) > 3<<20 {
				return nil, fmt.Errorf("YDB schema exceeds snapshot size limit")
			}
			if len(result.Tables) > 4096 {
				return nil, fmt.Errorf("YDB schema read exceeds table limit")
			}
		}
	}
	slices.SortFunc(result.Tables, func(a, b *protocol.Table) int {
		if compared := strings.Compare(a.Identity.Schema, b.Identity.Schema); compared != 0 {
			return compared
		}
		return strings.Compare(a.Identity.Name, b.Identity.Name)
	})
	if err := provider.ValidateSchema(result); err != nil {
		return nil, err
	}
	return result, nil
}

func component(value string) bool {
	return utf8.ValidString(value) && value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

func scopes(database string, selectors []string) ([]string, []*protocol.InspectionScopeBinding, error) {
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
	if len(described.GetChangefeeds()) != 0 {
		return nil, fmt.Errorf("YDB changefeeds require their child-object reader")
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
