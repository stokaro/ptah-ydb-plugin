package backend

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeedsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/declaration"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/replicationsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/streamingsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/topicsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/workloadsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// DecodeSource interprets captured YAML and Go declarations of native objects
// and row-table facets, and HCL declarations of coordination nodes.
// It reads no files or environment variables and requires no
// database session. Unsupported syntax or any invalid entry discards the
// complete result.
func (*Provider) DecodeSource(ctx context.Context, request *protocol.DecodeSourceRequest) (*protocol.DecodedSource, error) {
	if err := provider.ValidateSourceRequest(described(), request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result := &protocol.DecodedSource{Complete: true}
	for _, document := range request.Documents {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		decoded, err := decodeDocument(ctx, document)
		if err != nil {
			return nil, err
		}
		result.Documents = append(result.Documents, decoded)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidateDecodedSource(described(), request, result); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	return result, nil
}

// decodeDocument decodes one source document: the values of each fragment,
// and the coverage the document claims. Its error is the call's status.
func decodeDocument(ctx context.Context, document *protocol.SourceDocument) (*protocol.DecodedSourceDocument, error) {
	decoded := &protocol.DecodedSourceDocument{Id: document.Id, Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED}}
	// A source's ptah:not-described header, the HCL document's or the Go
	// source's, leaves a native kind or one object of it outside the
	// document's claim, as a YQL document's does: with source.limits
	// negotiated the claim below leaves what a limit names uninspected
	// (see limitedClaim). A limit no object can take is refused here with
	// the host's words.
	if err := yqlschema.CheckLimits(document.GetLimits()); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	for _, model := range document.Models {
		decoded.Coverage.Kinds = append(decoded.Coverage.Kinds, &protocol.ModelKindCoverage{Model: proto.CloneOf(model), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
	}
	kinds, err := decodeKinds(ctx, document)
	if err != nil {
		return nil, err
	}
	if err := refuseColumnTableChangefeeds(kinds.tables, kinds.feeds); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	for index, fragment := range document.Fragments {
		values, err := kinds.values(ctx, document, fragment, index)
		if err != nil {
			return nil, sourceError(ctx, err)
		}
		decoded.Fragments = append(decoded.Fragments, &protocol.DecodedSourceFragment{Id: fragment.Id, Values: values})
		for _, value := range values {
			value.Value.RequiredObjects, value.Value.RequiresDatabaseContext, err = schema.RequiredObjects(value.Value.Value)
			if err != nil {
				return nil, authored.Status(codes.InvalidArgument, err)
			}
			owner := value.Value.Value.Owner
			decoded.Coverage.Subjects = append(decoded.Coverage.Subjects, &protocol.ModelSubjectCoverage{
				Kind: value.Value.Value.Kind, Subject: proto.CloneOf(owner), NormalizedSubject: proto.CloneOf(owner),
				Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
			})
		}
	}
	if err := limitedClaim(ctx, document, decoded.Coverage); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return decoded, nil
}

// sourceKinds holds what each kind's decoder reads out of one source
// document: the values of the native objects by fragment index, and the
// tables, the indexes' own settings and the changefeeds the document
// declares.
type sourceKinds struct {
	tables       []tablesource.Table
	indexes      []tablesource.Index
	natives      []tablesource.NativeIndexes
	secrets      map[int][]*protocol.SourceModelValue
	topics       map[int][]*protocol.SourceModelValue
	feeds        []changefeedsource.Table
	externals    map[int][]*protocol.SourceModelValue
	workloads    map[int][]*protocol.SourceModelValue
	queries      map[int][]*protocol.SourceModelValue
	replications map[int][]*protocol.SourceModelValue
}

// decodeKinds runs every kind's decoder over document, in a fixed order, and
// stops at the first that fails. Its error is the call's status.
func decodeKinds(ctx context.Context, document *protocol.SourceDocument) (*sourceKinds, error) {
	var kinds sourceKinds
	var err error
	if kinds.tables, err = tablesource.Decode(ctx, document); err != nil {
		return nil, sourceError(ctx, err)
	}
	if kinds.indexes, err = tablesource.DecodeIndexes(ctx, document); err != nil {
		return nil, sourceError(ctx, err)
	}
	if kinds.natives, err = tablesource.DecodeNativeIndexes(ctx, document, kinds.tables); err != nil {
		return nil, sourceError(ctx, err)
	}
	// Without the feature the host reads no index's settings, so the
	// comparison could not see what an index holds, and a declared
	// setting would be dropped on every index the table already has.
	if len(kinds.indexes) != 0 && !slices.Contains(negotiatedFeatures(ctx), provider.ObservedIndexFacetsFeature) {
		return nil, authored.Status(codes.InvalidArgument, fmt.Errorf("index %q declares its partitioning or read replicas, which this provider compares only with a host that negotiates %s",
			kinds.indexes[0].Value.GetValue().GetOwner().GetName(), provider.ObservedIndexFacetsFeature))
	}
	if kinds.secrets, err = secretsource.Decode(ctx, document); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if kinds.topics, err = topicsource.Decode(ctx, document); err != nil {
		return nil, sourceError(ctx, err)
	}
	if kinds.feeds, err = changefeedsource.Decode(ctx, document); err != nil {
		return nil, sourceError(ctx, err)
	}
	if kinds.externals, err = externalsource.Decode(ctx, document); err != nil {
		return nil, sourceError(ctx, err)
	}
	if kinds.workloads, err = workloadsource.Decode(ctx, document); err != nil {
		return nil, sourceError(ctx, err)
	}
	if kinds.queries, err = streamingsource.Decode(ctx, document); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if kinds.replications, err = replicationsource.Decode(ctx, document); err != nil {
		return nil, sourceError(ctx, err)
	}
	return &kinds, nil
}

// values returns the values one fragment of the document declares, as the
// decoder of the kind that owns the fragment read them.
func (k *sourceKinds) values(ctx context.Context, document *protocol.SourceDocument, fragment *protocol.SourceFragment, index int) ([]*protocol.SourceModelValue, error) {
	switch {
	case secretsource.Owns(fragment):
		return k.secrets[index], nil
	case streamingsource.Owns(fragment):
		return k.queries[index], nil
	case topicsource.Owns(fragment):
		return k.topics[index], nil
	case replicationsource.Owns(fragment):
		return k.replications[index], nil
	case workloadsource.Owns(fragment):
		return k.workloads[index], nil
	case changefeedsource.Owns(fragment):
		return changefeedValues(document, fragment, index, k.feeds), nil
	case externalsource.Owns(fragment):
		return k.externals[index], nil
	default:
		return fragmentValues(ctx, document, fragment, index, k)
	}
}

// sourceError is the status of a failed decoding: the context's when the
// context has ended, and InvalidArgument otherwise.
func sourceError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return authored.Status(codes.InvalidArgument, err)
}

// limitedClaim leaves outside a document's claim what its ptah:not-described
// limits name, when the host negotiated source.limits. Without the feature the
// SDK lets a decoding claim only complete knowledge, and the limits change
// nothing. Nor does the SDK let a document without limits claim less, so an
// HCL document without a coordination_node entry claims every node here,
// where the host's embedded engine reads it as claiming none: an Atlas file
// names none of the nodes it leaves out. A host keeps that rule by taking no
// coverage from such a reply.
func limitedClaim(ctx context.Context, document *protocol.SourceDocument, coverage *protocol.ModelCoverage) error {
	if len(document.GetLimits()) == 0 || !slices.Contains(negotiatedFeatures(ctx), provider.SourceLimitsFeature) {
		return nil
	}
	return yqlschema.LimitCoverage(coverage, document.GetLimits())
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
// fragments declare one facet together, which its first fragment returns, and
// so do an index's, and the vector and full-text indexes of a table.
func fragmentValues(ctx context.Context, document *protocol.SourceDocument, fragment *protocol.SourceFragment, index int, kinds *sourceKinds) ([]*protocol.SourceModelValue, error) {
	if !tablesource.Owns(fragment) {
		return coordinationSource(ctx, document, fragment, index)
	}
	tables, indexes := kinds.tables, kinds.indexes
	for _, declared := range kinds.natives {
		if declared.Fragment == index {
			trace := &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT}
			return []*protocol.SourceModelValue{{Placement: protocol.SourceModelValue_FACET, Value: declared.Value, Source: trace}}, nil
		}
	}
	for _, declared := range indexes {
		if declared.Fragment == index {
			trace := &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT}
			return []*protocol.SourceModelValue{{Placement: protocol.SourceModelValue_FACET, Value: declared.Value, Source: trace}}, nil
		}
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
	switch {
	case document.Format == protocol.SourceDocument_HCL && fragment.Selector == schema.CoordinationHCLBlock().Selector:
		return hclCoordinationSource(document, fragment, index)
	case document.Format == protocol.SourceDocument_YAML && fragment.Selector == schema.CoordinationYAMLKey().Selector:
		return yamlCoordinationSource(ctx, document, fragment, index)
	case document.Format == protocol.SourceDocument_GO_ANNOTATIONS && fragment.Selector == schema.CoordinationGoDirective().Selector:
		return goCoordinationSource(document, fragment, index)
	default:
		return nil, fmt.Errorf("unsupported YDB coordination source attachment or format")
	}
}

// hclCoordinationSource reads the node one coordination_node block declares.
func hclCoordinationSource(document *protocol.SourceDocument, fragment *protocol.SourceFragment, index int) ([]*protocol.SourceModelValue, error) {
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
}

// yamlCoordinationSource reads the nodes a coordination_nodes mapping
// declares, in the order of their names.
func yamlCoordinationSource(ctx context.Context, document *protocol.SourceDocument, fragment *protocol.SourceFragment, index int) ([]*protocol.SourceModelValue, error) {
	mapping := fragment.GetValue().GetMapping()
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
}

// goCoordinationSource reads the node one Go directive declares. A directive
// declares one node with its attributes; the host lexed them, and every value
// is text.
func goCoordinationSource(document *protocol.SourceDocument, fragment *protocol.SourceFragment, index int) ([]*protocol.SourceModelValue, error) {
	mapping := fragment.GetValue().GetMapping()
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
