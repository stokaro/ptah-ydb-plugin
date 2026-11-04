package backend

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// EncodeSource writes DESIRED values back as the fragments this provider's
// claims read (contract 25.3, source.encode), offline. The host writes the
// syntax.
//
// A native object is written as one entry of its YAML key or one directive of
// its own with the directives of its children: a coordination node, a topic
// and its consumers, a secret by the variable its value comes from, an
// external data source and an external table, a resource pool and a
// classifier, and a streaming query. HCL declares a coordination node, in a
// coordination_node block, and no other object. The facets of one table are
// written together as the table declaration the table decoders read (see
// sourceencode.EncodeTable); HCL has no spelling for any of them, as the
// host's embedded engine reports. A Go export also gives each column the Go
// type of its YDB type, and loses a table with a nullable key column, which
// no declaration can ask for.
//
// A value that needs several parts is written through a group when the host
// negotiated source.encode-groups, and is a loss otherwise. A value no source
// spells, or one whose parts do not decode back to it, is a loss with one line
// that names it and never its content. Knowledge the request lacks is written
// as the source's not-described entries, so that reading the source back
// claims no more than the request knew; knowledge no entry can say is refused
// (see encodeCoverage).
func (*Provider) EncodeSource(ctx context.Context, request *protocol.EncodeSourceRequest) (*protocol.EncodedSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := provider.NegotiatedDescription(Description(), provider.HostFeatures(ctx))
	if err := provider.ValidateEncodeSourceRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	limits, err := encodeCoverage(request, description.GetEngines()[0], sourceLimitsNegotiated(description.GetEngines()[0]))
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	format := request.GetFormat()
	result := &protocol.EncodedSource{Complete: true, Limits: limits}
	var units []encodedUnit
	var nodes []encodedNode
	// subjects names each value as a loss line names it.
	subjects := make(map[string]string, len(request.GetValues()))
	tables := make(map[string]*sourceencode.Table)
	var order []string
	for _, value := range request.GetValues() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		envelope := value.GetValue().GetValue()
		id := value.GetValue().GetId()
		subjects[id] = envelope.GetOwner().GetKind() + " " + sourceencode.Subject(envelope.GetOwner())
		if value.GetPlacement() == protocol.SourceModelValue_NATIVE_OBJECT {
			subjects[id] = sourceencode.Noun(envelope.GetKind()) + " " + sourceencode.Subject(envelope.GetOwner())
		}
		switch {
		case value.GetPlacement() == protocol.SourceModelValue_FACET && format == protocol.SourceDocument_HCL:
			result.Losses = append(result.Losses, &protocol.EncodeLoss{ValueId: id, Message: lossMessage(format, envelope)})
		case value.GetPlacement() == protocol.SourceModelValue_FACET:
			table, bound := tables[value.GetBindingId()]
			if !bound {
				table = &sourceencode.Table{Binding: bindingOf(request, value.GetBindingId())}
				tables[value.GetBindingId()] = table
				order = append(order, value.GetBindingId())
			}
			table.Values = append(table.Values, value)
		case envelope.GetKind() == schema.CoordinationProperties:
			node, err := nodeOf(value.GetValue())
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			if node.access {
				result.Losses = append(result.Losses, &protocol.EncodeLoss{ValueId: node.id, Message: sourceencode.AccessLoss("coordination node", node.owner).Loss})
				continue
			}
			nodes = append(nodes, node)
		default:
			encoded, err := encodeNative(format, value.GetValue())
			if err != nil {
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			if encoded.Loss != "" {
				result.Losses = append(result.Losses, &protocol.EncodeLoss{ValueId: id, Message: encoded.Loss})
				continue
			}
			units = append(units, encodedUnit{ids: []string{id}, parts: encoded.Parts})
		}
	}
	slices.SortFunc(nodes, func(a, b encodedNode) int {
		return cmp.Or(strings.Compare(a.owner.GetSchema(), b.owner.GetSchema()), strings.Compare(a.owner.GetName(), b.owner.GetName()))
	})
	fragments, err := nodeFragments(format, nodes)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	for _, fragment := range fragments {
		units = append(units, encodedUnit{ids: fragment.GetValueIds(), parts: []*protocol.EncodedSourcePart{{Fragment: fragment.GetFragment(), Placement: fragment.GetPlacement()}}})
	}
	// A table with a column no declaration can ask for is lost whole. The
	// loss carries the reason to the host, which refuses a Go export with it;
	// an error's text does not reach the host.
	refused := nullableKeys(request)
	for _, binding := range order {
		if reason, found := refused[binding]; found {
			for _, value := range tables[binding].Values {
				result.Losses = append(result.Losses, &protocol.EncodeLoss{ValueId: value.GetValue().GetId(), Message: reason})
			}
			delete(refused, binding)
			continue
		}
		encoded, err := sourceencode.EncodeTable(format, *tables[binding])
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		result.Losses = append(result.Losses, encoded.Losses...)
		if len(encoded.Written) != 0 {
			units = append(units, encodedUnit{ids: encoded.Written, parts: encoded.Parts})
		}
	}
	// A table the request binds and holds no facet of has no value to carry
	// the loss.
	for _, binding := range slices.Sorted(maps.Keys(refused)) {
		return nil, status.Error(codes.FailedPrecondition, refused[binding])
	}
	if format == protocol.SourceDocument_GO_ANNOTATIONS {
		result.FieldTypes = fieldTypes(request)
	}
	grouped := slices.Contains(description.GetEngines()[0].GetFeatures(), provider.EncodeGroupsFeature)
	for _, unit := range units {
		switch {
		case grouped:
			result.Groups = append(result.Groups, &protocol.EncodedSourceGroup{ValueIds: unit.ids, Parts: unit.parts})
		case len(unit.parts) == 1:
			result.Fragments = append(result.Fragments, &protocol.EncodedFragment{ValueIds: unit.ids, Fragment: unit.parts[0].GetFragment(), Placement: unit.parts[0].GetPlacement()})
		default:
			for _, id := range unit.ids {
				result.Losses = append(result.Losses, &protocol.EncodeLoss{ValueId: id,
					Message: fmt.Sprintf("%s needs several declarations, which a host that does not negotiate %s cannot write", subjects[id], provider.EncodeGroupsFeature)})
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidateEncodedSource(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}

// encodedUnit is what writes some values together: the values, and the
// ordered parts that write them.
type encodedUnit struct {
	ids   []string
	parts []*protocol.EncodedSourcePart
}

// bindingOf is the request's binding of id. The SDK checked that a facet's
// binding exists.
func bindingOf(request *protocol.EncodeSourceRequest, id string) *protocol.SourceBinding {
	for _, binding := range request.GetBindings() {
		if binding.GetId() == id {
			return binding
		}
	}
	return nil
}

// encodeNative writes one native object other than a coordination node.
func encodeNative(format protocol.SourceDocument_Format, value *protocol.NormalizedValue) (sourceencode.Result, error) {
	encoders := map[string]func(protocol.SourceDocument_Format, *protocol.NormalizedValue) (sourceencode.Result, error){
		schema.SecretProperties:                 sourceencode.Secret,
		schema.ExternalSourceProperties:         sourceencode.ExternalSource,
		schema.ExternalTableProperties:          sourceencode.ExternalTable,
		schema.TopicProperties:                  sourceencode.Topic,
		schema.ResourcePoolProperties:           sourceencode.ResourcePool,
		schema.ResourcePoolClassifierProperties: sourceencode.ResourcePoolClassifier,
		schema.StreamingQueryProperties:         sourceencode.StreamingQuery,
	}
	encode, known := encoders[value.GetValue().GetKind()]
	if !known {
		return sourceencode.Result{Loss: lossMessage(format, value.GetValue())}, nil
	}
	return encode(format, value)
}

// nullableKeys says, by binding, why a table of a Go export cannot be
// written: it has a nullable key column. YDB takes one, but no declaration can
// ask for it, since the provider writes NOT NULL on every key column it
// creates, and written as an ordinary key field the column would be rebuilt
// NOT NULL wherever the models are applied. The first such column of a table
// is named.
func nullableKeys(request *protocol.EncodeSourceRequest) map[string]string {
	refused := make(map[string]string)
	for _, column := range request.GetColumns() {
		if _, named := refused[column.GetBindingId()]; named || !column.GetNullable() || !column.GetPrimaryKey() {
			continue
		}
		table := bindingOf(request, column.GetBindingId()).GetObject()
		refused[column.GetBindingId()] = fmt.Sprintf("column %q of table %q is a nullable key column, which no Ptah declaration can represent: "+
			"Ptah writes NOT NULL on every YDB key column", column.GetName(), path.Join(table.GetSchema(), table.GetName()))
	}
	return refused
}

// fieldTypes gives each column of a Go export the Go type of its YDB type,
// and leaves a type it does not know to the host.
func fieldTypes(request *protocol.EncodeSourceRequest) []*protocol.EncodedFieldType {
	var types []*protocol.EncodedFieldType
	for _, column := range request.GetColumns() {
		if goType, known := ydbtype.GoTypeOf(column.GetType()); known {
			types = append(types, &protocol.EncodedFieldType{BindingId: column.GetBindingId(), Column: column.GetName(), GoType: goType.Name, GoImport: goType.Import})
		}
	}
	return types
}

// encodedNode is a coordination node a request asks to write, with the
// settings a source spells.
type encodedNode struct {
	id       string
	owner    *protocol.Reference
	settings []*protocol.SourceEntry
	// structName is where a YAML source declared the node.
	structName string
	access     bool
}

// nodeOf reads a node's settings out of its desired value, spelled as a
// source spells them, and checks that a source decodes them back to the same
// value, its access and its struct name aside.
func nodeOf(value *protocol.NormalizedValue) (encodedNode, error) {
	envelope := value.GetValue()
	var intent schema.CoordinationIntent
	decoder := json.NewDecoder(bytes.NewReader(envelope.GetPayload()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return encodedNode{}, fmt.Errorf("invalid YDB coordination node value")
	}
	node := encodedNode{id: value.GetId(), owner: proto.CloneOf(envelope.GetOwner()), structName: intent.StructName, access: intent.Access != nil}
	text := func(key, value string) {
		node.settings = append(node.settings, &protocol.SourceEntry{Key: key, Value: &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}})
	}
	if period := intent.SelfCheckPeriodMillis; period != nil {
		text("self_check_period", ydbtype.IntervalText(int64(*period)*1_000))
	}
	if period := intent.SessionGracePeriodMillis; period != nil {
		text("session_grace_period", ydbtype.IntervalText(int64(*period)*1_000))
	}
	for _, mode := range []struct {
		key   string
		value *string
	}{{"read_consistency_mode", intent.ReadConsistencyMode}, {"attach_consistency_mode", intent.AttachConsistencyMode}, {"rate_limiter_counters_mode", intent.RateLimiterCountersMode}} {
		if mode.value != nil {
			text(mode.key, *mode.value)
		}
	}
	// The source reads the settings back into the value it came from.
	entries := slices.Clone(node.settings)
	entries = append(entries, &protocol.SourceEntry{Key: "name", Value: &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: node.owner.GetName()}}})
	if node.owner.GetSchema() != "" {
		entries = append(entries, &protocol.SourceEntry{Key: "schema", Value: &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: node.owner.GetSchema()}}})
	}
	_, decoded, err := coordinationEntry(&protocol.SourceEntry{Value: &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: entries}}}})
	if err != nil {
		return encodedNode{}, fmt.Errorf("YDB coordination node %s: %w", path.Join(node.owner.GetSchema(), node.owner.GetName()), err)
	}
	intent.Access, intent.StructName = nil, ""
	if !equalIntents(decoded, intent) {
		return encodedNode{}, fmt.Errorf("YDB coordination node %s does not read back as written", path.Join(node.owner.GetSchema(), node.owner.GetName()))
	}
	return node, nil
}

func equalIntents(a, b schema.CoordinationIntent) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}

// nodeFragments writes the nodes in a format: one YAML fragment holding every
// node, or one Go directive or HCL block for each.
func nodeFragments(format protocol.SourceDocument_Format, nodes []encodedNode) ([]*protocol.EncodedFragment, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	text := func(value string) *protocol.SourceValue {
		return &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}
	}
	identity := func(node encodedNode) []*protocol.SourceEntry {
		entries := []*protocol.SourceEntry{{Key: "name", Value: text(node.owner.GetName())}}
		if node.owner.GetSchema() != "" {
			entries = append(entries, &protocol.SourceEntry{Key: "schema", Value: text(node.owner.GetSchema())})
		}
		return entries
	}
	switch format {
	case protocol.SourceDocument_YAML:
		claim := schema.CoordinationYAMLKey()
		mapping := &protocol.SourceMapping{}
		fragment := &protocol.EncodedFragment{}
		for _, node := range nodes {
			entries := slices.Concat(identity(node), node.settings)
			if node.structName != "" {
				entries = append(entries, &protocol.SourceEntry{Key: "struct_name", Value: text(node.structName)})
			}
			mapping.Entries = append(mapping.Entries, &protocol.SourceEntry{Key: path.Join(node.owner.GetSchema(), node.owner.GetName()),
				Value: &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: entries}}}})
			fragment.ValueIds = append(fragment.ValueIds, node.id)
		}
		fragment.Fragment = &protocol.SourceFragment{Id: "coordination_nodes", Selector: claim.GetSelector(), YamlKey: claim,
			Content: &protocol.SourceFragment_Value{Value: &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: mapping}}}}
		return []*protocol.EncodedFragment{fragment}, nil
	case protocol.SourceDocument_GO_ANNOTATIONS:
		claim := schema.CoordinationGoDirective()
		fragments := make([]*protocol.EncodedFragment, 0, len(nodes))
		for _, node := range nodes {
			fragments = append(fragments, &protocol.EncodedFragment{ValueIds: []string{node.id}, Fragment: &protocol.SourceFragment{
				Id: node.id, Selector: claim.GetSelector(), GoDirective: claim, Content: &protocol.SourceFragment_Value{Value: &protocol.SourceValue{
					Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: slices.Concat(identity(node), node.settings)}}}}}})
		}
		return fragments, nil
	case protocol.SourceDocument_HCL:
		claim := schema.CoordinationHCLBlock()
		fragments := make([]*protocol.EncodedFragment, 0, len(nodes))
		for _, node := range nodes {
			labels := []*protocol.SourceLabel{{Value: node.owner.GetName()}}
			if node.owner.GetSchema() != "" {
				labels = []*protocol.SourceLabel{{Value: node.owner.GetSchema()}, {Value: node.owner.GetName()}}
			}
			fragments = append(fragments, &protocol.EncodedFragment{ValueIds: []string{node.id}, Placement: protocol.EncodedFragment_AFTER_TABLES,
				Fragment: &protocol.SourceFragment{Id: node.id, Selector: claim.GetSelector(), HclBlock: claim, Content: &protocol.SourceFragment_Block{
					Block: &protocol.SourceBlock{Type: claim.GetType(), Labels: labels, Attributes: &protocol.SourceMapping{Entries: slices.Clone(node.settings)}}}}})
		}
		return fragments, nil
	default:
		return nil, fmt.Errorf("unsupported source format %s", format)
	}
}

// lossMessage says, in one line, why a value is not written, naming it and
// never its content: a table's facet in HCL, which has no spelling for one,
// or a native object no source of the provider declares.
func lossMessage(format protocol.SourceDocument_Format, envelope *protocol.Envelope) string {
	owner := envelope.GetOwner()
	subject := sourceencode.Subject(owner)
	switch envelope.GetKind() {
	case schema.TableProperties, schema.ColumnStorageProperties, schema.ColumnProperties:
		return fmt.Sprintf("the YDB settings of %s %s: HCL has no spelling for a YDB table's partitioning, layout, TTL, column families or column storage", owner.GetKind(), subject)
	case schema.IndexProperties, schema.ChangefeedProperties:
		return fmt.Sprintf("the %s of %s %s: HCL has no spelling for a YDB table's native indexes or changefeeds",
			strings.ReplaceAll(strings.TrimPrefix(envelope.GetKind(), schema.ProviderID+"/table-"), "-", " "), owner.GetKind(), subject)
	}
	if format == protocol.SourceDocument_HCL {
		return sourceencode.HCLLoss(envelope).Loss
	}
	return fmt.Sprintf("%s %s: this provider writes no source for it", strings.ReplaceAll(strings.TrimPrefix(envelope.GetKind(), schema.ProviderID+"/"), "-", " "), subject)
}

var _ provider.SourceEncoder = (*Provider)(nil)
