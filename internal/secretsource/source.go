// Package secretsource decodes value references from YAML and Go declarations.
package secretsource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
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
// The refusal is a reason a host prints (see authored): it names the secret
// and the attribute and says what the attribute takes, as the host's embedded
// engine did, and never quotes the value a declaration wrote or the variable
// it names.
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
			return nil, refusal(entry.name, "a secret is declared as a mapping of name, schema and value_env")
		}
		values, err := secretAttributes(entry.name, entry.fields)
		if err != nil {
			return nil, err
		}
		name := entry.name
		if value, exists := values["name"]; exists {
			name = value
		}
		directory := values["schema"]
		if err := checkIdentity(entry.name, directory, name); err != nil {
			return nil, err
		}
		if err := secrets.CheckValueEnv(values["value_env"]); err != nil {
			declared, ok := errors.AsType[*secrets.DeclarationError](err)
			if !ok {
				return nil, err
			}
			named := declared.Reason
			if declared.Variable != "" {
				named = "the variable it names " + named
			}
			return nil, refusal(entry.name, "invalid "+declared.Attribute+": "+named)
		}
		path := secrets.Display(directory, name)
		if seen[path] {
			return nil, refusal(entry.name, "secret "+strconv.Quote(path)+" is declared twice")
		}
		seen[path] = true
		if declaresTable(document, directory, name) {
			return nil, refusal(entry.name, "secret "+strconv.Quote(path)+" is at the path of a table the document declares")
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

// secretAttributes reads the text attributes of the secret keyed key, trimmed.
// An inline value, an unknown or repeated attribute, or one that is not text
// refuses the secret without repeating the value.
func secretAttributes(key string, fields *protocol.SourceMapping) (map[string]string, error) {
	values := make(map[string]string)
	for _, field := range fields.Entries {
		if field.Key == "value" {
			return nil, refusal(key, "invalid value: a secret's value is never written in a schema file; "+
				"name the environment variable that holds it with value_env")
		}
		if field.Key != "name" && field.Key != "schema" && field.Key != "value_env" {
			return nil, refusal(key, "unknown attribute "+strconv.Quote(field.Key)+"; a secret takes name, schema and value_env")
		}
		if _, exists := values[field.Key]; exists {
			return nil, refusal(key, "attribute "+field.Key+" is written twice")
		}
		text, ok := field.GetValue().GetValue().(*protocol.SourceValue_Text)
		if !ok {
			return nil, refusal(key, "invalid "+field.Key+": it takes text")
		}
		values[field.Key] = strings.TrimSpace(text.Text)
	}
	return values, nil
}

// checkIdentity refuses the directory and the name the secret keyed key
// declares where they are not an exact path, in the order and the words of the
// host's embedded engine.
func checkIdentity(key, directory, name string) error {
	if secrets.ValidateIdentity(directory, name) == nil {
		return nil
	}
	switch {
	case strings.TrimSpace(name) == "":
		return refusal(key, "invalid name: a secret needs a name")
	case strings.Contains(name, "/"):
		return refusal(key, "invalid name: "+strconv.Quote(name)+" holds a slash; name the directory with schema")
	case secrets.ValidateIdentity("", name) != nil:
		return refusal(key, "invalid name: "+strconv.Quote(name)+" is not a path segment")
	case strings.HasPrefix(directory, "/"):
		return refusal(key, "invalid schema: "+strconv.Quote(directory)+" starts with a slash; "+
			"name the directory relative to the database root, without the database's own path")
	default:
		return refusal(key, "invalid schema: "+strconv.Quote(directory)+" is not a directory path relative to the database root")
	}
}

// refusal is the reason the declaration of the secret keyed key is refused for,
// the words of the host's embedded engine: `secret "<key>": text`. A Go
// directive keys no secret, and the host names the directive's place in front
// of the reason, so key is "" there and text stands alone, as it does when the
// key would make the reason longer than a host shows.
func refusal(key, text string) error {
	if key == "" {
		return authored.New(text)
	}
	keyed := "secret " + strconv.Quote(key) + ": " + text
	if provider.ValidateDiagnostic(&protocol.ProviderDiagnostic{Reason: keyed}) != nil {
		return authored.New(text)
	}
	return authored.New(keyed)
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
