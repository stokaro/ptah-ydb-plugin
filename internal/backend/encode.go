package backend

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// EncodeSource writes DESIRED values back as the fragments this provider's
// claims read (contract 25.3, source.encode), offline: a coordination node as
// one entry of the coordination_nodes YAML key, one ptah:schema:coordinationnode
// directive, or one coordination_node HCL block, which decodes to the same
// node. The host writes the syntax.
//
// A node's access is left out, as every source leaves it: omission keeps a
// node's access unmanaged. Its struct name, which records where a YAML source
// declared it and has no server counterpart, is written in YAML alone.
//
// Every other value is reported as a loss. This provider holds a table's YDB
// settings, its partitioning, layout, TTL, families and column storage, as one
// value, which a source writes as several declarations, and the contract
// writes one value with one fragment; HCL has no spelling for them at all, as
// the host's embedded engine reports. Indexes, changefeeds and the other
// native objects have no source of this provider.
func (*Provider) EncodeSource(ctx context.Context, request *protocol.EncodeSourceRequest) (*protocol.EncodedSource, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := provider.NegotiatedDescription(Description(), provider.HostFeatures(ctx))
	if err := provider.ValidateEncodeSourceRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result := &protocol.EncodedSource{Complete: true}
	var nodes []encodedNode
	for _, value := range request.GetValues() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		envelope := value.GetValue().GetValue()
		if envelope.GetKind() != schema.CoordinationProperties || value.GetPlacement() != protocol.SourceModelValue_NATIVE_OBJECT {
			result.Losses = append(result.Losses, &protocol.EncodeLoss{ValueId: value.GetValue().GetId(), Message: lossMessage(request.GetFormat(), envelope)})
			continue
		}
		node, err := nodeOf(value.GetValue())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		nodes = append(nodes, node)
	}
	slices.SortFunc(nodes, func(a, b encodedNode) int {
		return cmp.Or(strings.Compare(a.owner.GetSchema(), b.owner.GetSchema()), strings.Compare(a.owner.GetName(), b.owner.GetName()))
	})
	fragments, err := nodeFragments(request.GetFormat(), nodes)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	result.Fragments = fragments
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidateEncodedSource(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}

// encodedNode is a coordination node a request asks to write, with the
// settings a source spells.
type encodedNode struct {
	id       string
	owner    *protocol.Reference
	settings []*protocol.SourceEntry
	// structName is where a YAML source declared the node.
	structName string
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
	node := encodedNode{id: value.GetId(), owner: proto.CloneOf(envelope.GetOwner()), structName: intent.StructName}
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
// never its content.
func lossMessage(format protocol.SourceDocument_Format, envelope *protocol.Envelope) string {
	owner := envelope.GetOwner()
	subject := path.Join(slices.Concat([]string{owner.GetSchema()}, owner.GetPath(), []string{owner.GetName()})...)
	switch envelope.GetKind() {
	case schema.TableProperties, schema.ColumnStorageProperties, schema.ColumnProperties:
		if format == protocol.SourceDocument_HCL {
			return fmt.Sprintf("the YDB settings of %s %s: HCL has no spelling for a YDB table's partitioning, layout, TTL, column families or column storage", owner.GetKind(), subject)
		}
		return fmt.Sprintf("the YDB settings of %s %s: this provider holds a table's settings as one value, which a source writes as several declarations", owner.GetKind(), subject)
	default:
		return fmt.Sprintf("%s %s: this provider writes no source for it", strings.ReplaceAll(strings.TrimPrefix(envelope.GetKind(), schema.ProviderID+"/"), "-", " "), subject)
	}
}

var _ provider.SourceEncoder = (*Provider)(nil)
