// Package secretsource decodes value references from YAML and Go declarations.
package secretsource

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// Directive is the document-level secret annotation.
const Directive = "ptah:schema:secret"

// YAMLKey declares the document's secret mapping.
func YAMLKey() *protocol.YAMLSourceKey {
	return &protocol.YAMLSourceKey{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: "secrets", Selector: "secrets", Models: []string{schema.SecretProperties}}
}

// GoDirective declares a value-free secret annotation.
func GoDirective() *protocol.GoSourceDirective {
	return &protocol.GoSourceDirective{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: Directive, Selector: Directive, Attributes: []string{"name", "schema", "value_env", "value"}, Models: []string{schema.SecretProperties}}
}

// Owns identifies fragments assigned to this decoder.
func Owns(fragment *protocol.SourceFragment) bool {
	return (fragment.GetYamlKey().GetScope() == protocol.YAMLSourceKey_DOCUMENT && fragment.GetYamlKey().GetKey() == "secrets") || fragment.GetGoDirective().GetDirective() == Directive
}

// Decode preserves exact paths and reference names. Any invalid entry rejects
// the complete document without repeating a possible inline value in errors.
func Decode(ctx context.Context, document *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error) {
	result := make(map[int][]*protocol.SourceModelValue)
	seen := make(map[string]bool)
	for index, fragment := range document.Fragments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) {
			continue
		}
		if fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
			return nil, fmt.Errorf("unsupported secret source attachment")
		}
		type entry struct {
			name   string
			fields *protocol.SourceMapping
			path   []*protocol.SourcePathStep
		}
		var entries []entry
		mapping := fragment.GetValue().GetMapping()
		if mapping == nil {
			return nil, fmt.Errorf("secret declarations require a mapping")
		}
		switch {
		case document.Format == protocol.SourceDocument_YAML && fragment.Selector == "secrets":
			fields := slices.Clone(mapping.Entries)
			slices.SortFunc(fields, func(a, b *protocol.SourceEntry) int { return strings.Compare(a.Key, b.Key) })
			for _, field := range fields {
				entries = append(entries, entry{field.Key, field.GetValue().GetMapping(), []*protocol.SourcePathStep{{Step: &protocol.SourcePathStep_Key{Key: field.Key}}}})
			}
		case document.Format == protocol.SourceDocument_GO_ANNOTATIONS && fragment.Selector == Directive:
			entries = append(entries, entry{fields: mapping})
		default:
			return nil, fmt.Errorf("unsupported secret source format")
		}
		result[index] = nil
		for position, entry := range entries {
			if entry.fields == nil {
				return nil, fmt.Errorf("a secret requires a mapping")
			}
			values := map[string]string{}
			for _, field := range entry.fields.Entries {
				if field.Key == "value" {
					return nil, fmt.Errorf("a secret declaration must name value_env instead of an inline value")
				}
				if field.Key != "name" && field.Key != "schema" && field.Key != "value_env" {
					return nil, fmt.Errorf("unknown secret declaration attribute")
				}
				if _, exists := values[field.Key]; exists {
					return nil, fmt.Errorf("duplicate secret declaration attribute")
				}
				text, ok := field.GetValue().GetValue().(*protocol.SourceValue_Text)
				if !ok {
					return nil, fmt.Errorf("secret declaration attributes require text")
				}
				values[field.Key] = strings.TrimSpace(text.Text)
			}
			name := entry.name
			if value, exists := values["name"]; exists {
				name = value
			}
			directory := values["schema"]
			if err := secrets.ValidateIdentity(directory, name); err != nil {
				return nil, err
			}
			if err := secrets.CheckValueEnv(values["value_env"]); err != nil {
				return nil, fmt.Errorf("secret value_env requires a valid PTAH_SECRET_ input name")
			}
			path := secrets.Display(directory, name)
			if seen[path] {
				return nil, fmt.Errorf("a secret path is declared twice")
			}
			seen[path] = true
			for _, binding := range document.Bindings {
				ref := binding.GetObject()
				if ref.GetSchema() == directory && ref.GetName() == name {
					return nil, fmt.Errorf("secret conflicts with a declared table")
				}
			}
			owner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Schema: directory, Name: name}
			value, err := schema.CaptureSecretIntent(owner, &native.SecretIntent{ValueEnv: values["value_env"]})
			if err != nil {
				return nil, err
			}
			result[index] = append(result[index], &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
				Value:  &protocol.NormalizedValue{Id: "secret." + strconv.Itoa(index) + "." + strconv.Itoa(position), Value: value, NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: schema.NativeDesiredModel(schema.SecretProperties).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)},
				Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT, Path: entry.path}})
		}
	}
	return result, ctx.Err()
}
