// Package sourceencode writes loss-accounted fragments for provider-owned models.
package sourceencode

import (
	"bytes"
	"context"
	"fmt"
	"path"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// Result contains either complete syntax parts or one reason the format cannot
// preserve a valid value. The caller owns value accounting and document assembly.
type Result struct {
	Parts []*protocol.EncodedSourcePart
	Loss  string
}

type decoder func(context.Context, *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error)

func text(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}
}

func mapping(entries ...*protocol.SourceEntry) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: entries}}}
}

func field(key, value string) *protocol.SourceEntry {
	return &protocol.SourceEntry{Key: key, Value: text(value)}
}

func localIdentity(value *protocol.NormalizedValue) ([]*protocol.SourceEntry, Result, error) {
	owner := value.GetValue().GetOwner()
	if err := schema.ValidateNativeOwner(owner); err != nil || !proto.Equal(owner, value.GetNormalizedOwner()) {
		return nil, Result{}, fmt.Errorf("source value requires its exact validated native identity")
	}
	if owner.GetDatabase() != "" {
		return nil, Result{Loss: "A database-qualified identity requires an established source binding"}, nil
	}
	fields := []*protocol.SourceEntry{field("name", owner.GetName())}
	if owner.GetSchema() != "" {
		fields = append(fields, field("schema", owner.GetSchema()))
	}
	return fields, Result{}, nil
}

func nativePart(format protocol.SourceDocument_Format, value *protocol.NormalizedValue, fields []*protocol.SourceEntry,
	yaml *protocol.YAMLSourceKey, annotation *protocol.GoSourceDirective, decode decoder,
) (Result, error) {
	fragment := &protocol.SourceFragment{Id: "native." + value.GetId()}
	switch format {
	case protocol.SourceDocument_YAML:
		owner := value.GetValue().GetOwner()
		fragment.Selector, fragment.YamlKey = yaml.GetSelector(), proto.CloneOf(yaml)
		fragment.Content = &protocol.SourceFragment_Value{Value: mapping(&protocol.SourceEntry{
			Key: path.Join(owner.GetSchema(), owner.GetName()), Value: mapping(fields...),
		})}
	case protocol.SourceDocument_GO_ANNOTATIONS:
		fragment.Selector, fragment.GoDirective = annotation.GetSelector(), proto.CloneOf(annotation)
		fragment.Content = &protocol.SourceFragment_Value{Value: mapping(fields...)}
	case protocol.SourceDocument_HCL:
		return Result{Loss: "HCL has no declaration for this native model"}, nil
	default:
		return Result{}, fmt.Errorf("unsupported native source format")
	}
	// A valid native payload may contain facts the source grammar cannot say.
	// Re-read the candidate before returning any part; omission is a loss.
	document := &protocol.SourceDocument{Id: "encoded", Format: format, Fragments: []*protocol.SourceFragment{fragment}}
	decoded, err := decode(context.Background(), document)
	if err != nil || len(decoded[0]) != 1 {
		return Result{Loss: "The source grammar cannot preserve this native declaration"}, nil
	}
	before, err := schema.NormalizeDesired(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	after, err := schema.NormalizeDesired(decoded[0][0].GetValue().GetValue())
	if err != nil || !proto.Equal(before.GetOwner(), after.GetOwner()) || !bytes.Equal(before.GetPayload(), after.GetPayload()) {
		return Result{Loss: "The source grammar cannot preserve all native declaration facts"}, nil
	}
	return Result{Parts: []*protocol.EncodedSourcePart{{Fragment: fragment}}}, nil
}
