// Package sourceencode writes this provider's desired values as the YAML keys
// and Go directives its source decoders read back. It is the encoding half of
// the source.encode service: internal/backend accounts for every value of a
// request and assembles the reply, and this package decides, for one value or
// for the facets of one table, which syntax parts write it or why none can.
//
// A value is written as the declaration the provider compares it as. Before
// any part is returned, the candidate parts are decoded by the decoder of the
// same claims and the declaration they give is compared with the value; a
// value that does not come back is a loss, never a partial write. What no
// declaration states is never written: a secret's value, an access
// requirement, or a setting a read reports that no YQL statement sets.
package sourceencode

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"path"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Result holds the syntax parts that write one value together, or the one
// line that says why the format cannot preserve it. The caller owns value
// accounting and the shape of the reply.
type Result struct {
	Parts []*protocol.EncodedSourcePart
	Loss  string
}

// loss is a Result that writes nothing, for the reason given.
func loss(format string, arguments ...any) Result {
	return Result{Loss: fmt.Sprintf(format, arguments...)}
}

type decoder func(context.Context, *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error)

// sameValue reports whether a decoded value states what the encoded one does.
type sameValue func(before, after *protocol.Envelope) (bool, error)

func text(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}
}

func mapping(entries ...*protocol.SourceEntry) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: entries}}}
}

func field(key, value string) *protocol.SourceEntry {
	return &protocol.SourceEntry{Key: key, Value: text(value)}
}

func sortedFields(values map[string]string) []*protocol.SourceEntry {
	fields := make([]*protocol.SourceEntry, 0, len(values))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		fields = append(fields, field(key, values[key]))
	}
	return fields
}

// Subject names a value's owner as a loss line names it: its directory and
// its name, or for a column its table and its name.
func Subject(owner *protocol.Reference) string {
	return path.Join(slices.Concat([]string{owner.GetSchema()}, owner.GetPath(), []string{owner.GetName()})...)
}

// AccessLoss is the line that reports a value's access requirement. No source
// format spells an owner or a permission set, and a declaration without one
// leaves the object's access unmanaged, so writing the rest would drop a
// requirement the value states.
func AccessLoss(noun string, owner *protocol.Reference) Result {
	return loss("the access requirement of %s %s has no source spelling", noun, Subject(owner))
}

// localIdentity is the name and directory a source declares an object by. An
// owner a read bound to its database names the database the value was read
// in: a source names no database, and the host binds a declaration to the one
// it connects to, so that database is left out. Any other database cannot be
// spelled.
func localIdentity(value *protocol.NormalizedValue) ([]*protocol.SourceEntry, Result, error) {
	owner := value.GetValue().GetOwner()
	validate := schema.ValidateNativeOwner
	if schema.IsWorkloadObject(owner.GetKind()) {
		validate = schema.ValidateWorkloadOwner
	}
	if err := validate(owner); err != nil || !proto.Equal(owner, value.GetNormalizedOwner()) {
		return nil, Result{}, fmt.Errorf("source value requires its exact validated native identity")
	}
	if database := owner.GetDatabase(); database != "" && database != value.GetValue().GetContext().GetDatabase() {
		return nil, loss("%s %s names database %s, which a source cannot name", Noun(owner.GetKind()), Subject(owner), database), nil
	}
	fields := []*protocol.SourceEntry{field("name", owner.GetName())}
	if owner.GetSchema() != "" {
		fields = append(fields, field("schema", owner.GetSchema()))
	}
	return fields, Result{}, nil
}

// Noun is what a loss line calls a value of kind.
func Noun(kind string) string {
	switch kind {
	case schema.TopicProperties:
		return "topic"
	case schema.SecretProperties:
		return "secret"
	case schema.ExternalSourceProperties:
		return "external data source"
	case schema.ExternalTableProperties:
		return "external table"
	case schema.ResourcePoolProperties:
		return "resource pool"
	case schema.ResourcePoolClassifierProperties:
		return "resource pool classifier"
	case schema.StreamingQueryProperties:
		return "streaming query"
	case schema.CoordinationProperties:
		return "coordination node"
	case schema.ViewProperties:
		return "view"
	default:
		return "object"
	}
}

// exact reports a decoded value with the canonical payload of the encoded one.
func exact(before, after *protocol.Envelope) (bool, error) {
	return bytes.Equal(before.GetPayload(), after.GetPayload()), nil
}

// nativePart writes fields as the YAML key or Go directive of a native object
// and keeps them when what they decode to is the same declaration as value,
// which same decides.
func nativePart(format protocol.SourceDocument_Format, value *protocol.NormalizedValue, fields []*protocol.SourceEntry,
	yaml *protocol.YAMLSourceKey, annotation *protocol.GoSourceDirective, decode decoder, same sameValue,
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
		return HCLLoss(value.GetValue()), nil
	default:
		return Result{}, fmt.Errorf("unsupported native source format")
	}
	return nativeParts(value, format, []*protocol.SourceFragment{fragment}, decode, same)
}

// HCLLoss reports a native object HCL declares no block for: the provider
// claims a coordination_node block only, as the host's embedded engine does.
func HCLLoss(value *protocol.Envelope) Result {
	return loss("%s %s: HCL has no block that declares it", Noun(value.GetKind()), Subject(value.GetOwner()))
}

// nativeParts re-reads the candidate fragments of one native object and
// returns them as its parts when they give back exactly one declaration of
// the same object, which same reports as stating what the value does. A valid
// payload may hold facts the grammar cannot say; omitting one is a loss.
func nativeParts(value *protocol.NormalizedValue, format protocol.SourceDocument_Format, fragments []*protocol.SourceFragment, decode decoder, same sameValue) (Result, error) {
	owner := value.GetValue().GetOwner()
	unreadable := loss("%s %s: the source grammar cannot preserve this declaration", Noun(owner.GetKind()), Subject(owner))
	document := &protocol.SourceDocument{Id: "encoded", Format: format, Fragments: fragments}
	decoded, err := decode(context.Background(), document)
	if err != nil || len(decoded[0]) != 1 {
		return unreadable, nil
	}
	for index, values := range decoded {
		if index != 0 && len(values) != 0 {
			return unreadable, nil
		}
	}
	before, err := schema.NormalizeDesired(value.GetValue())
	if err != nil {
		return Result{}, err
	}
	after, err := schema.NormalizeDesired(decoded[0][0].GetValue().GetValue())
	if err != nil {
		return unreadable, nil
	}
	local := proto.CloneOf(before.GetOwner())
	local.Database = ""
	equal, err := same(before, after)
	if err != nil {
		return Result{}, err
	}
	if !proto.Equal(local, after.GetOwner()) || !equal {
		return loss("%s %s: the source grammar cannot preserve every setting of this declaration", Noun(owner.GetKind()), Subject(owner)), nil
	}
	parts := make([]*protocol.EncodedSourcePart, len(fragments))
	for i, fragment := range fragments {
		parts[i] = &protocol.EncodedSourcePart{Fragment: fragment}
	}
	return Result{Parts: parts}, nil
}
