// Package streamingsource decodes the streaming queries declared by a YAML
// document or Go file. It retains the query text, execution settings, and
// explicit permission to reset aggregation state.
package streamingsource

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

const (
	// KeyQueries is the YAML document key that declares streaming queries.
	KeyQueries = "streaming_queries"
	// DirectiveQuery declares one streaming query in Go.
	DirectiveQuery = "ptah:schema:streamingquery"
)

func attributes() []string {
	return []string{"name", "schema", "text", "run", "resource_pool", "allow_state_reset"}
}

// YAMLKey declares the streaming_queries document key.
func YAMLKey() *protocol.YAMLSourceKey {
	return &protocol.YAMLSourceKey{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: KeyQueries, Selector: KeyQueries, Models: []string{schema.StreamingQueryProperties}}
}

// GoDirectives declares the document-scoped streaming query annotation.
func GoDirectives() []*protocol.GoSourceDirective {
	return []*protocol.GoSourceDirective{{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectiveQuery, Selector: DirectiveQuery,
		Attributes: attributes(), Models: []string{schema.StreamingQueryProperties}}}
}

// Owns reports whether a fragment carries this package's document claim.
func Owns(fragment *protocol.SourceFragment) bool {
	if key := fragment.GetYamlKey(); key != nil {
		return key.GetScope() == protocol.YAMLSourceKey_DOCUMENT && key.GetKey() == KeyQueries
	}
	directive := fragment.GetGoDirective()
	return directive.GetScope() == protocol.GoSourceDirective_DOCUMENT && directive.GetDirective() == DirectiveQuery
}

type declared struct {
	fragment        int
	path            []*protocol.SourcePathStep
	directory       string
	name            string
	spec            streaming.Spec
	allowStateReset bool
}

// Decode reads the document's claimed fragments, indexed by their positions.
// An invalid declaration or duplicate identity refuses the whole document.
func Decode(ctx context.Context, document *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := map[int][]*protocol.SourceModelValue{}
	var objects []declared
	for index, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) {
			continue
		}
		if fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
			return nil, fmt.Errorf("unsupported YDB streaming query source attachment")
		}
		var read []declared
		switch {
		case document.Format == protocol.SourceDocument_YAML && fragment.GetYamlKey() != nil && fragment.Selector == KeyQueries:
			mapping := fragment.GetValue().GetMapping()
			if mapping == nil && fragment.GetValue().GetNull() == nil {
				return nil, fmt.Errorf("%s requires a mapping", KeyQueries)
			}
			entries := slices.Clone(mapping.GetEntries())
			slices.SortFunc(entries, func(a, b *protocol.SourceEntry) int { return strings.Compare(a.GetKey(), b.GetKey()) })
			for position, entry := range entries {
				if position > 0 && entry.Key == entries[position-1].Key {
					return nil, fmt.Errorf("%s: duplicate key %q", KeyQueries, entry.Key)
				}
				object, err := query(entry.Key, entry.GetValue().GetMapping(), false)
				if err != nil {
					return nil, fmt.Errorf("streaming query %q: %w", entry.Key, err)
				}
				object.path = []*protocol.SourcePathStep{{Step: &protocol.SourcePathStep_Key{Key: entry.Key}}}
				read = append(read, object)
			}
		case document.Format == protocol.SourceDocument_GO_ANNOTATIONS && fragment.GetGoDirective() != nil && fragment.Selector == DirectiveQuery:
			object, err := query("", fragment.GetValue().GetMapping(), true)
			if err != nil {
				return nil, fmt.Errorf("%w on //%s", err, DirectiveQuery)
			}
			read = append(read, object)
		default:
			return nil, fmt.Errorf("unsupported YDB streaming query source format")
		}
		result[index] = nil
		for _, object := range read {
			object.fragment = index
			objects = append(objects, object)
		}
	}
	seen := map[string]bool{}
	for position, object := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		identity := streaming.Display(object.directory, object.name)
		if seen[identity] {
			return nil, fmt.Errorf("streaming query %q is declared twice", identity)
		}
		seen[identity] = true
		owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: object.directory, Name: object.name}
		value, err := schema.CaptureStreamingQueryIntent(owner, object.spec, object.allowStateReset)
		if err != nil {
			return nil, fmt.Errorf("streaming query %q: %w", identity, err)
		}
		fragment := document.Fragments[object.fragment]
		result[object.fragment] = append(result[object.fragment], &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
			Value: &protocol.NormalizedValue{Id: "streaming." + strconv.Itoa(position), Value: value, NormalizedOwner: proto.CloneOf(owner),
				CanonicalFormat: schema.NativeDesiredModel(schema.StreamingQueryProperties).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)},
			Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT, Path: object.path}})
	}
	return result, nil
}

func query(name string, mapping *protocol.SourceMapping, goSource bool) (declared, error) {
	if mapping == nil {
		return declared{}, fmt.Errorf("a streaming query requires a mapping")
	}
	object := declared{name: name}
	seen := map[string]bool{}
	for _, entry := range mapping.GetEntries() {
		if !slices.Contains(attributes(), entry.Key) {
			return declared{}, fmt.Errorf("unknown key %q", entry.Key)
		}
		if seen[entry.Key] {
			return declared{}, fmt.Errorf("duplicate key %q", entry.Key)
		}
		seen[entry.Key] = true
		value := entry.GetValue()
		if goSource {
			if _, ok := value.GetValue().(*protocol.SourceValue_Text); !ok {
				return declared{}, fmt.Errorf("attribute %s requires text", entry.Key)
			}
		}
		if entry.Key == "run" || entry.Key == "allow_state_reset" {
			flag, err := boolean(value, goSource)
			if err != nil {
				return declared{}, fmt.Errorf("%s: %w", entry.Key, err)
			}
			if entry.Key == "run" {
				object.spec.Run = flag
			} else {
				object.allowStateReset = flag != nil && *flag
			}
			continue
		}
		text, err := scalar(value)
		if err != nil {
			return declared{}, fmt.Errorf("%s: %w", entry.Key, err)
		}
		switch entry.Key {
		case "name":
			if text != "" {
				object.name = text
			}
		case "schema":
			object.directory = text
		case "text":
			object.spec.Text = text
		case "resource_pool":
			object.spec.ResourcePool = text
		}
	}
	if err := streaming.Validate(object.spec); err != nil {
		return declared{}, err
	}
	if err := streaming.ValidateIdentity(object.directory, object.name); err != nil {
		return declared{}, err
	}
	return object, nil
}

// scalar follows the host's yamlvalue.Scalar grammar, including null as empty.
func scalar(value *protocol.SourceValue) (string, error) {
	switch content := value.GetValue().(type) {
	case *protocol.SourceValue_Text:
		return content.Text, nil
	case *protocol.SourceValue_Number:
		return content.Number, nil
	case *protocol.SourceValue_Boolean:
		return strconv.FormatBool(content.Boolean), nil
	case *protocol.SourceValue_Null:
		return "", nil
	default:
		return "", fmt.Errorf("expected a scalar")
	}
}

// boolean keeps each frontend's grammar: Go uses ParseBool, while the YAML
// decoder accepts typed booleans, null, and the YAML 1.1 boolean words.
func boolean(value *protocol.SourceValue, goSource bool) (*bool, error) {
	if goSource {
		flag, err := strconv.ParseBool(value.GetText())
		if err != nil {
			return nil, fmt.Errorf("expected a boolean")
		}
		return &flag, nil
	}
	switch content := value.GetValue().(type) {
	case *protocol.SourceValue_Boolean:
		return new(content.Boolean), nil
	case *protocol.SourceValue_Null:
		return nil, nil
	case *protocol.SourceValue_Text:
		switch content.Text {
		case "y", "Y", "yes", "Yes", "YES", "on", "On", "ON":
			return new(true), nil
		case "n", "N", "no", "No", "NO", "off", "Off", "OFF":
			return new(false), nil
		}
	}
	return nil, fmt.Errorf("expected a boolean")
}
