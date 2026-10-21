package backend

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/declaration"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// DecodeSource interprets complete captured YAML coordination declarations.
// It reads no files or environment variables and requires no database session.
// Unsupported syntax or any invalid entry discards the complete result.
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
		for _, model := range document.Models {
			decoded.Coverage.Kinds = append(decoded.Coverage.Kinds, &protocol.ModelKindCoverage{Model: proto.CloneOf(model), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
		}
		for index, fragment := range document.Fragments {
			values, err := coordinationSource(ctx, document, fragment, index)
			if err != nil {
				if ctx.Err() != nil {
					return nil, status.FromContextError(ctx.Err()).Err()
				}
				return nil, status.Error(codes.InvalidArgument, err.Error())
			}
			decoded.Fragments = append(decoded.Fragments, &protocol.DecodedSourceFragment{Id: fragment.Id, Values: values})
			for _, value := range values {
				owner := value.Value.Value.Owner
				decoded.Coverage.Subjects = append(decoded.Coverage.Subjects, &protocol.ModelSubjectCoverage{Kind: owner.Kind, Subject: proto.CloneOf(owner), NormalizedSubject: proto.CloneOf(owner), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
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

func coordinationSource(ctx context.Context, document *protocol.SourceDocument, fragment *protocol.SourceFragment, index int) ([]*protocol.SourceModelValue, error) {
	if document.Format != protocol.SourceDocument_YAML || fragment.Selector != "coordination_nodes" || fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
		return nil, fmt.Errorf("unsupported YDB coordination source attachment or format")
	}
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
		owner, intent, err := coordinationEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("coordination node %q: %w", entry.Key, err)
		}
		for _, binding := range document.Bindings {
			object := binding.GetObject()
			if object.GetKind() == "table" && object.GetDatabase() == owner.Database && object.GetCatalog() == "" && object.GetSchema() == owner.Schema && object.GetName() == owner.Name {
				return nil, fmt.Errorf("YDB coordination node conflicts with a declared table")
			}
		}
		value, err := schema.CaptureCoordinationIntent(owner, intent)
		if err != nil {
			return nil, err
		}
		values = append(values, &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
			Value: &protocol.NormalizedValue{Id: strconv.Itoa(index) + "." + strconv.Itoa(entryIndex), Value: value, NormalizedOwner: proto.CloneOf(owner),
				CanonicalFormat: schema.CoordinationDesiredModel().CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)},
			Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT, Path: []*protocol.SourcePathStep{{Step: &protocol.SourcePathStep_Key{Key: entry.Key}}}},
		})
	}
	return values, nil
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
