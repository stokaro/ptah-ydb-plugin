// Package secretsource decodes value references from YAML and Go declarations.
package secretsource

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
		decoded, err := fragmentSecrets(document, index, seen)
		if err != nil {
			return nil, err
		}
		result[index] = decoded
	}
	return result, ctx.Err()
}

// secretEntry is one secret a fragment declares: its key name, if the format
// keys it, its attributes, and its path below the fragment.
type secretEntry struct {
	name   string
	fields *protocol.SourceMapping
	path   []*protocol.SourcePathStep
}

// fragmentSecrets returns the values of the secrets the claimed fragment at
// index declares, in entry order. seen holds the paths earlier fragments
// declared, and a path declared twice refuses the document.
func fragmentSecrets(document *protocol.SourceDocument, index int, seen map[string]bool) ([]*protocol.SourceModelValue, error) {
	fragment := document.Fragments[index]
	if fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
		return nil, fmt.Errorf("unsupported secret source attachment")
	}
	entries, err := secretEntries(document, fragment)
	if err != nil {
		return nil, err
	}
	var decoded []*protocol.SourceModelValue
	for position, entry := range entries {
		if entry.fields == nil {
			return nil, fmt.Errorf("a secret requires a mapping")
		}
		values, err := secretAttributes(entry.fields)
		if err != nil {
			return nil, err
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
		if declaresTable(document, directory, name) {
			return nil, fmt.Errorf("secret conflicts with a declared table")
		}
		owner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Schema: directory, Name: name}
		value, err := schema.CaptureSecretIntent(owner, &native.SecretIntent{ValueEnv: values["value_env"]})
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
			Value: &protocol.NormalizedValue{
				Id: "secret." + strconv.Itoa(index) + "." + strconv.Itoa(position), Value: value, NormalizedOwner: proto.CloneOf(owner),
				CanonicalFormat: schema.NativeDesiredModel(schema.SecretProperties).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload),
			},
			Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT, Path: entry.path}})
	}
	return decoded, nil
}

// secretEntries lists the secrets of a fragment: each entry of a YAML secrets
// mapping in key order, keyed by its name, or the one secret of a Go directive.
func secretEntries(document *protocol.SourceDocument, fragment *protocol.SourceFragment) ([]secretEntry, error) {
	var entries []secretEntry
	mapping := fragment.GetValue().GetMapping()
	if mapping == nil {
		return nil, fmt.Errorf("secret declarations require a mapping")
	}
	switch {
	case document.Format == protocol.SourceDocument_YAML && fragment.Selector == "secrets":
		fields := slices.Clone(mapping.Entries)
		slices.SortFunc(fields, func(a, b *protocol.SourceEntry) int { return strings.Compare(a.Key, b.Key) })
		for _, field := range fields {
			entries = append(entries, secretEntry{field.Key, field.GetValue().GetMapping(), []*protocol.SourcePathStep{{Step: &protocol.SourcePathStep_Key{Key: field.Key}}}})
		}
	case document.Format == protocol.SourceDocument_GO_ANNOTATIONS && fragment.Selector == Directive:
		entries = append(entries, secretEntry{fields: mapping})
	default:
		return nil, fmt.Errorf("unsupported secret source format")
	}
	return entries, nil
}

// secretAttributes reads the text attributes of one secret, trimmed. An inline
// value, an unknown or repeated attribute, or one that is not text refuses the
// secret without repeating the value.
func secretAttributes(fields *protocol.SourceMapping) (map[string]string, error) {
	values := make(map[string]string)
	for _, field := range fields.Entries {
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
	return values, nil
}

// declaresTable reports whether the document binds a table at the secret's
// directory and name.
func declaresTable(document *protocol.SourceDocument, directory, name string) bool {
	for _, binding := range document.Bindings {
		ref := binding.GetObject()
		if ref.GetSchema() == directory && ref.GetName() == name {
			return true
		}
	}
	return false
}
