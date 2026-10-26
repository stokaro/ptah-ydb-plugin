// Package workloadsource decodes the resource pools and classifiers a YAML
// document or a Go file declares, with the syntax and refusals of the host's
// bundled YDB source (internal/ydbsource): the YAML document keys
// resource_pools and resource_pool_classifiers, and the Go directives
// ptah:schema:resourcepool and ptah:schema:resourcepool:classifier.
package workloadsource

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// The syntax this package claims.
const (
	// KeyPools is the YAML document key that declares resource pools.
	KeyPools = "resource_pools"
	// KeyClassifiers is the YAML document key that declares classifiers.
	KeyClassifiers = "resource_pool_classifiers"
	// DirectivePool declares one resource pool in Go.
	DirectivePool = "ptah:schema:resourcepool"
	// DirectiveClassifier declares one classifier in Go.
	DirectiveClassifier = "ptah:schema:resourcepool:classifier"
)

// YAMLKeys declares the YAML document keys of pools and classifiers.
func YAMLKeys() []*protocol.YAMLSourceKey {
	return []*protocol.YAMLSourceKey{
		{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: KeyPools, Selector: KeyPools, Models: []string{schema.ResourcePoolProperties}},
		{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: KeyClassifiers, Selector: KeyClassifiers, Models: []string{schema.ResourcePoolClassifierProperties}},
	}
}

// GoDirectives declares the Go directives of a pool and of a classifier, with
// the attributes of the host's own directives of the same names.
func GoDirectives() []*protocol.GoSourceDirective {
	return []*protocol.GoSourceDirective{
		{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectivePool, Selector: DirectivePool,
			Attributes: workload.PoolAttributes(), Models: []string{schema.ResourcePoolProperties}},
		{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectiveClassifier, Selector: DirectiveClassifier,
			Attributes: workload.ClassifierAttributes(), Models: []string{schema.ResourcePoolClassifierProperties}},
	}
}

// Owns reports whether a fragment carries one of this package's claims.
func Owns(fragment *protocol.SourceFragment) bool {
	if key := fragment.GetYamlKey(); key != nil {
		return key.GetScope() == protocol.YAMLSourceKey_DOCUMENT && (key.GetKey() == KeyPools || key.GetKey() == KeyClassifiers)
	}
	directive := fragment.GetGoDirective().GetDirective()
	return directive == DirectivePool || directive == DirectiveClassifier
}

// declared is one pool or classifier a document declares, with the fragment
// and path its trace points at.
type declared struct {
	fragment   int
	path       []*protocol.SourcePathStep
	classifier bool
	name       string
	settings   proto.Message
}

// Decode reads every pool and classifier a document declares and returns the
// values of each fragment, by index. An invalid declaration refuses the whole
// document, and so does an object declared twice.
func Decode(ctx context.Context, document *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error) {
	var objects []declared
	owned := map[int]bool{}
	for index, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) {
			continue
		}
		owned[index] = true
		if fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
			return nil, fmt.Errorf("unsupported YDB workload source attachment")
		}
		mapping := fragment.GetValue().GetMapping()
		var read []declared
		var err error
		switch {
		case document.Format == protocol.SourceDocument_YAML && (fragment.Selector == KeyPools || fragment.Selector == KeyClassifiers):
			read, err = yamlObjects(index, fragment.Selector == KeyClassifiers, mapping)
		case document.Format == protocol.SourceDocument_GO_ANNOTATIONS && (fragment.Selector == DirectivePool || fragment.Selector == DirectiveClassifier):
			var object declared
			object, err = goObject(fragment.Selector == DirectiveClassifier, mapping)
			object.fragment = index
			read = []declared{object}
		default:
			err = fmt.Errorf("unsupported YDB workload source format")
		}
		if err != nil {
			return nil, err
		}
		objects = append(objects, read...)
	}
	result := make(map[int][]*protocol.SourceModelValue, len(owned))
	for index := range owned {
		result[index] = nil
	}
	seen := map[string]bool{}
	for position, object := range objects {
		kind, noun := schema.ResourcePoolProperties, "resource pool"
		if object.classifier {
			kind, noun = schema.ResourcePoolClassifierProperties, "resource pool classifier"
		}
		if seen[kind+"\x00"+object.name] {
			return nil, fmt.Errorf("%s %q is declared twice", noun, object.name)
		}
		seen[kind+"\x00"+object.name] = true
		owner := &protocol.Reference{Engine: "ydb", Kind: kind, Name: object.name}
		value, err := schema.CaptureWorkloadIntent(owner, object.settings)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", noun, object.name, err)
		}
		fragment := document.Fragments[object.fragment]
		result[object.fragment] = append(result[object.fragment], &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
			Value: &protocol.NormalizedValue{Id: "workload." + strconv.Itoa(position), Value: value, NormalizedOwner: proto.CloneOf(owner),
				CanonicalFormat: schema.NativeDesiredModel(kind).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)},
			Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT, Path: object.path}})
	}
	return result, nil
}

// yamlObjects reads a resource_pools or resource_pool_classifiers mapping:
// each entry an object keyed by its name, in key order.
func yamlObjects(fragment int, classifier bool, mapping *protocol.SourceMapping) ([]declared, error) {
	key, noun, allowed := KeyPools, "resource pool", workload.PoolAttributes()[1:]
	if classifier {
		key, noun, allowed = KeyClassifiers, "resource pool classifier", workload.ClassifierAttributes()[1:]
	}
	if mapping == nil {
		return nil, fmt.Errorf("%s requires a mapping", key)
	}
	entries := slices.Clone(mapping.GetEntries())
	slices.SortFunc(entries, func(a, b *protocol.SourceEntry) int { return strings.Compare(a.Key, b.Key) })
	var result []declared
	for _, entry := range entries {
		fields := entry.GetValue().GetMapping()
		if fields == nil && entry.GetValue().GetNull() == nil {
			return nil, fmt.Errorf("%s %q: a %s requires a mapping", noun, entry.Key, noun)
		}
		values := map[string]string{}
		for _, field := range fields.GetEntries() {
			if !slices.Contains(allowed, field.Key) {
				return nil, fmt.Errorf("%s %q: unknown key %q", noun, entry.Key, field.Key)
			}
			text, err := scalar(field.GetValue())
			if err != nil {
				return nil, fmt.Errorf("%s %q: %s: %w", noun, entry.Key, field.Key, err)
			}
			values[field.Key] = text
		}
		values[workload.AttributeName] = entry.Key
		object, err := parse(classifier, values)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", noun, entry.Key, err)
		}
		object.fragment = fragment
		object.path = []*protocol.SourcePathStep{{Step: &protocol.SourcePathStep_Key{Key: entry.Key}}}
		result = append(result, object)
	}
	return result, nil
}

// goObject reads one directive's attributes, which the host lexed as text.
func goObject(classifier bool, mapping *protocol.SourceMapping) (declared, error) {
	directive := DirectivePool
	if classifier {
		directive = DirectiveClassifier
	}
	if mapping == nil {
		return declared{}, fmt.Errorf("%s requires attributes", directive)
	}
	values := map[string]string{}
	for _, entry := range mapping.GetEntries() {
		text, ok := entry.GetValue().GetValue().(*protocol.SourceValue_Text)
		if !ok {
			return declared{}, fmt.Errorf("attribute %s on %s requires text", entry.Key, directive)
		}
		values[entry.Key] = text.Text
	}
	object, err := parse(classifier, values)
	if err != nil {
		return declared{}, fmt.Errorf("%w on //%s", err, directive)
	}
	return object, nil
}

// parse reads one pool or classifier with the host's rules and words.
func parse(classifier bool, values map[string]string) (declared, error) {
	if classifier {
		name, spec, err := workload.ParseClassifier(values)
		if err != nil {
			return declared{}, err
		}
		return declared{classifier: true, name: name, settings: schema.ClassifierSettings(spec)}, nil
	}
	name, spec, err := workload.ParsePool(values)
	if err != nil {
		return declared{}, err
	}
	return declared{name: name, settings: schema.PoolSettings(spec)}, nil
}

// scalar reads a YAML scalar as the text the host's bundled source reads it
// as: a number keeps its digits and a null is a value written empty, which
// the setting's parse refuses.
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
