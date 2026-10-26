package backend

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeedsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/declaration"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// DecodeSource interprets captured YAML and Go declarations of native objects
// and row-table facets, and HCL declarations of coordination nodes.
// It reads no files or environment variables and requires no
// database session. Unsupported syntax or any invalid entry discards the
// complete result.
func (*Provider) DecodeSource(ctx context.Context, request *protocol.DecodeSourceRequest) (*protocol.DecodedSource, error) {
	if err := provider.ValidateSourceRequest(Description(), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result := &protocol.DecodedSource{Complete: true}
	for _, document := range request.Documents {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		decoded := &protocol.DecodedSourceDocument{Id: document.Id, Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED}}
		// An HCL document's ptah:not-described header leaves coordination
		// nodes, or one of them, outside its claim, as a YQL document's does.
		// The host applies the limits; a limit no node can take is refused here
		// with the host's words.
		if err := yqlschema.CheckLimits(document.GetLimits()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		for _, model := range document.Models {
			decoded.Coverage.Kinds = append(decoded.Coverage.Kinds, &protocol.ModelKindCoverage{Model: proto.CloneOf(model), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
		}
		tables, err := tablesource.Decode(ctx, document)
		if err != nil {
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		topics, err := topicsource.Decode(ctx, document)
		if err != nil {
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		feeds, err := changefeedsource.Decode(ctx, document)
		if err != nil {
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		externals, err := externalsource.Decode(ctx, document)
		if err != nil {
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		workloads, err := workloadsource.Decode(ctx, document)
		if err != nil {
			if ctx.Err() != nil {
				return nil, status.FromContextError(ctx.Err()).Err()
			}
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		queries, err := streamingsource.Decode(ctx, document)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if err := refuseColumnTableChangefeeds(tables, feeds); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		for index, fragment := range document.Fragments {
			var values []*protocol.SourceModelValue
			var err error
			switch {
			case streamingsource.Owns(fragment):
				values = queries[index]
			case topicsource.Owns(fragment):
				values = topics[index]
			case workloadsource.Owns(fragment):
				values = workloads[index]
			case changefeedsource.Owns(fragment):
				values = changefeedValues(document, fragment, index, feeds)
			case externalsource.Owns(fragment):
				values = externals[index]
			default:
				values, err = fragmentValues(ctx, document, fragment, index, tables)
			}
			if err != nil {
				if ctx.Err() != nil {
					return nil, status.FromContextError(ctx.Err()).Err()
				}
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			decoded.Fragments = append(decoded.Fragments, &protocol.DecodedSourceFragment{Id: fragment.Id, Values: values})
			for _, value := range values {
				value.Value.RequiredObjects, err = schema.RequiredObjects(value.Value.Value)
				if err != nil {
					return nil, status.Error(codes.InvalidArgument, err.Error())
				}
				owner := value.Value.Value.Owner
				decoded.Coverage.Subjects = append(decoded.Coverage.Subjects, &protocol.ModelSubjectCoverage{Kind: value.Value.Value.Kind, Subject: proto.CloneOf(owner), NormalizedSubject: proto.CloneOf(owner), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
			}
		}
		result.Documents = append(result.Documents, decoded)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidateDecodedSource(Description(), request, result); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return result, nil
}

// refuseColumnTableChangefeeds refuses a changefeed of a table the document
// declares as a column table: YDB keeps changefeeds on row tables only, and
// the host's bundled source refuses the declaration with these words.
func refuseColumnTableChangefeeds(tables []tablesource.Table, feeds []changefeedsource.Table) error {
	for _, feed := range feeds {
		for _, table := range tables {
			if table.Binding.GetId() == feed.Binding.GetId() && table.Value.GetValue().GetKind() == schema.ColumnStorageProperties {
				return fmt.Errorf("table %q: %s", tableName(feed.Binding.GetObject()), schema.ColumnTableRowSettingsRefusal)
			}
		}
	}
	return nil
}

// tableName names a table as the source refusals name it.
func tableName(table *protocol.Reference) string {
	if table.GetSchema() == "" {
		return table.GetName()
	}
	return table.GetSchema() + "." + table.GetName()
}

// changefeedValues returns the changefeed facet a table's first changefeed
// fragment declares, and nothing for its other fragments.
func changefeedValues(document *protocol.SourceDocument, fragment *protocol.SourceFragment, index int, feeds []changefeedsource.Table) []*protocol.SourceModelValue {
	for _, table := range feeds {
		if table.Fragment == index {
			return []*protocol.SourceModelValue{{Placement: protocol.SourceModelValue_FACET, Value: table.Value,
				Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT}}}
		}
	}
	return nil
}

// fragmentValues returns the values one fragment declares. A table's
// fragments declare one facet together, which its first fragment returns.
func fragmentValues(ctx context.Context, document *protocol.SourceDocument, fragment *protocol.SourceFragment, index int, tables []tablesource.Table) ([]*protocol.SourceModelValue, error) {
	if !tablesource.Owns(fragment) {
		return coordinationSource(ctx, document, fragment, index)
	}
	for _, table := range tables {
		trace := &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT}
		var values []*protocol.SourceModelValue
		if table.Fragment == index {
			values = append(values, &protocol.SourceModelValue{Placement: protocol.SourceModelValue_FACET, Value: table.Value, Source: trace})
		}
		if table.ColumnFragment == index {
			for _, column := range table.Columns {
				values = append(values, &protocol.SourceModelValue{Placement: protocol.SourceModelValue_FACET, Value: column, Source: proto.CloneOf(trace)})
			}
		}
		if len(values) != 0 {
			return values, nil
		}
	}
	return nil, nil
}

func coordinationSource(ctx context.Context, document *protocol.SourceDocument, fragment *protocol.SourceFragment, index int) ([]*protocol.SourceModelValue, error) {
	if fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
		return nil, fmt.Errorf("unsupported YDB coordination source attachment or format")
	}
	mapping := fragment.GetValue().GetMapping()
	switch {
	case document.Format == protocol.SourceDocument_HCL && fragment.Selector == schema.CoordinationHCLBlock().Selector:
		// The block's labels name the node: its name, or its directory and
		// its name. The frontend evaluated every attribute.
		block := fragment.GetBlock()
		labels := block.GetLabels()
		if len(labels) == 0 || len(labels) > 2 {
			return nil, fmt.Errorf("a coordination_node block is labeled with its name, or its directory and its name")
		}
		name, entries := labels[len(labels)-1].GetValue(), slices.Clone(block.GetAttributes().GetEntries())
		entries = append(entries, &protocol.SourceEntry{Key: "name", Value: &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: name}}})
		if len(labels) == 2 {
			entries = append(entries, &protocol.SourceEntry{Key: "schema", Value: &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: labels[0].GetValue()}}})
		}
		if len(block.GetChildren()) != 0 {
			return nil, fmt.Errorf("coordination node %q: a coordination_node block holds no block", name)
		}
		value, err := coordinationValue(document, fragment, &protocol.SourceEntry{Value: &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{
			Mapping: &protocol.SourceMapping{Entries: entries}}}}, strconv.Itoa(index), nil)
		if err != nil {
			return nil, fmt.Errorf("coordination node %q: %w", name, err)
		}
		return []*protocol.SourceModelValue{value}, nil
	case document.Format == protocol.SourceDocument_YAML && fragment.Selector == schema.CoordinationYAMLKey().Selector:
		if mapping == nil {
			return nil, fmt.Errorf("coordination_nodes requires a mapping")
		}
		entries := slices.Clone(mapping.Entries)
		slices.SortFunc(entries, func(a, b *protocol.SourceEntry) int { return strings.Compare(a.Key, b.Key) })
		var values []*protocol.SourceModelValue
		for entryIndex, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			value, err := coordinationValue(document, fragment, entry, strconv.Itoa(index)+"."+strconv.Itoa(entryIndex), []*protocol.SourcePathStep{{Step: &protocol.SourcePathStep_Key{Key: entry.Key}}})
			if err != nil {
				return nil, fmt.Errorf("coordination node %q: %w", entry.Key, err)
			}
			values = append(values, value)
		}
		return values, nil
	case document.Format == protocol.SourceDocument_GO_ANNOTATIONS && fragment.Selector == schema.CoordinationGoDirective().Selector:
		// A directive declares one node with its attributes; the host lexed
		// them, and every value is text.
		if mapping == nil {
			return nil, fmt.Errorf("%s requires attributes", schema.CoordinationGoDirective().Directive)
		}
		name := ""
		for _, entry := range mapping.Entries {
			if entry.Key == "name" {
				name = entry.GetValue().GetText()
			}
		}
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%s needs a name", schema.CoordinationGoDirective().Directive)
		}
		value, err := coordinationValue(document, fragment, &protocol.SourceEntry{Value: fragment.GetValue()}, strconv.Itoa(index), nil)
		if err != nil {
			return nil, fmt.Errorf("coordination node %q: %w", name, err)
		}
		return []*protocol.SourceModelValue{value}, nil
	default:
		return nil, fmt.Errorf("unsupported YDB coordination source attachment or format")
	}
}

// coordinationValue reads one declared node. A node may not take the path of
// a table the same document declares.
func coordinationValue(document *protocol.SourceDocument, fragment *protocol.SourceFragment, entry *protocol.SourceEntry, id string, path []*protocol.SourcePathStep) (*protocol.SourceModelValue, error) {
	owner, intent, err := coordinationEntry(entry)
	if err != nil {
		return nil, err
	}
	for _, binding := range document.Bindings {
		object := binding.GetObject()
		if object.GetKind() == "table" && object.GetDatabase() == owner.Database && object.GetCatalog() == "" && object.GetSchema() == owner.Schema && object.GetName() == owner.Name {
			return nil, fmt.Errorf("YDB coordination node conflicts with a declared table")
		}
	}
	if err := schema.ValidateManagedCoordinationOwner(owner); err != nil {
		return nil, err
	}
	value, err := schema.CaptureCoordinationIntent(owner, intent)
	if err != nil {
		return nil, err
	}
	return &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
		Value: &protocol.NormalizedValue{Id: id, Value: value, NormalizedOwner: proto.CloneOf(owner),
			CanonicalFormat: schema.CoordinationDesiredModel().CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)},
		Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT, Path: path},
	}, nil
}

func coordinationEntry(entry *protocol.SourceEntry) (*protocol.Reference, schema.CoordinationIntent, error) {
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Name: entry.Key}
	var intent schema.CoordinationIntent
	mapping := entry.GetValue().GetMapping()
	if mapping == nil {
		return nil, intent, fmt.Errorf("node settings require a mapping")
	}
	for _, field := range mapping.Entries {
		value, ok := field.GetValue().GetValue().(*protocol.SourceValue_Text)
		if !ok {
			return nil, intent, fmt.Errorf("%s requires text; null, expressions and numeric coercion are refused", field.Key)
		}
		switch field.Key {
		case "name":
			owner.Name = value.Text
		case "schema":
			owner.Schema = value.Text
		case "struct_name":
			intent.StructName = value.Text
		case "self_check_period", "session_grace_period":
			period, err := declaration.CoordinationPeriod(value.Text)
			if err != nil {
				return nil, intent, fmt.Errorf("%s: %w", field.Key, err)
			}
			if field.Key == "self_check_period" {
				intent.SelfCheckPeriodMillis = &period
			} else {
				intent.SessionGracePeriodMillis = &period
			}
		case "read_consistency_mode":
			mode := strings.ToLower(strings.TrimSpace(value.Text))
			intent.ReadConsistencyMode = &mode
		case "attach_consistency_mode":
			mode := strings.ToLower(strings.TrimSpace(value.Text))
			intent.AttachConsistencyMode = &mode
		case "rate_limiter_counters_mode":
			mode := strings.ToLower(strings.TrimSpace(value.Text))
			intent.RateLimiterCountersMode = &mode
		default:
			return nil, intent, fmt.Errorf("unknown setting %q", field.Key)
		}
	}
	return owner, intent, nil
}

var _ provider.SourceDecoder = (*Provider)(nil)
