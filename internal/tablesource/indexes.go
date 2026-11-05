package tablesource

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Index is the facet a document's fragments declare for one index: its
// partitioning and its read replicas.
type Index struct {
	// Fragment is the index in the document of the first fragment that
	// declared a setting of the index. The facet is returned with it, and the
	// index's other fragments return no value.
	Fragment int
	// Value is the normalized desired facet, owned by the index.
	Value *protocol.NormalizedValue
}

type pendingIndex struct {
	subject  *protocol.Reference
	fragment int
	intent   native.IndexSettingsIntent
}

// DecodeIndexes reads the settings every index directive and index entry of
// document declares, one facet per index, in the order of each index's first
// fragment. An index option the provider does not carry from a source is
// refused by name, and so is a value the setting cannot take, where it was
// written. It reads no file and no environment.
func DecodeIndexes(ctx context.Context, document *protocol.SourceDocument) ([]Index, error) {
	var order []string
	byIndex := make(map[string]*pendingIndex)
	for position, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) || !indexFragment(fragment) {
			continue
		}
		entries, err := indexEntries(fragment)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			continue
		}
		subject := fragment.GetSubject()
		key := subject.GetSchema() + "\x00" + subject.GetPath()[0] + "\x00" + subject.GetName()
		target, seen := byIndex[key]
		if !seen {
			target = &pendingIndex{subject: subject, fragment: position}
			byIndex[key] = target
			order = append(order, key)
		}
		for _, entry := range entries {
			if err := target.set(entry.key, entry.value); err != nil {
				return nil, fmt.Errorf("index %q: %w", subject.GetName(), err)
			}
		}
	}
	var indexes []Index
	for _, key := range order {
		target := byIndex[key]
		value, err := target.normalized(document)
		if err != nil {
			return nil, fmt.Errorf("index %q: %w", target.subject.GetName(), err)
		}
		indexes = append(indexes, Index{Fragment: target.fragment, Value: value})
	}
	return indexes, nil
}

// indexFragment reports whether a fragment is an index directive, an index
// entry's key or the attributes of an HCL index block.
func indexFragment(fragment *protocol.SourceFragment) bool {
	return fragment.GetGoDirective().GetDirective() == DirectiveIndex || fragment.GetYamlKey().GetScope() == protocol.YAMLSourceKey_INDEX ||
		fragment.GetHclBlock().GetSite() == protocol.HCLSourceBlock_INDEX_ATTRIBUTES
}

type indexEntry struct{ key, value string }

// indexEntries returns the settings one index fragment writes, in the order it
// writes them, and refuses the first option the provider does not carry.
func indexEntries(fragment *protocol.SourceFragment) ([]indexEntry, error) {
	if fragment.GetSubject().GetKind() != "index" || len(fragment.GetSubject().GetPath()) != 1 {
		return nil, fmt.Errorf("fragment %q: an index setting needs the index it belongs to", fragment.GetId())
	}
	if key := fragment.GetYamlKey(); key != nil {
		if !slices.Contains(IndexSettingsAttributes(), key.GetKey()) {
			return nil, &UnsupportedError{Setting: key.GetKey(), Reason: indexReason}
		}
		text, err := scalar(fragment.GetValue())
		if errors.Is(err, errNull) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key.GetKey(), err)
		}
		return []indexEntry{{key: key.GetKey(), value: text}}, nil
	}
	var entries []indexEntry
	for _, entry := range fragment.GetValue().GetMapping().GetEntries() {
		if !slices.Contains(IndexSettingsAttributes(), entry.GetKey()) {
			return nil, &UnsupportedError{Setting: entry.GetKey(), Reason: indexReason}
		}
		text, err := scalar(entry.GetValue())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.GetKey(), err)
		}
		entries = append(entries, indexEntry{key: entry.GetKey(), value: text})
	}
	return entries, nil
}

// set reads one setting with the rules a table's partitioning attribute is
// read with.
func (p *pendingIndex) set(attribute, raw string) error {
	var parsed settings
	if err := parsed.setPartitioning(attribute, raw); err != nil {
		return err
	}
	if partitioning := parsed.intent.GetPartitioningSettings(); partitioning != nil {
		if p.intent.PartitioningSettings == nil {
			p.intent.PartitioningSettings = partitioning
		} else {
			proto.Merge(p.intent.PartitioningSettings, partitioning)
		}
	}
	if replicas := parsed.intent.GetReadReplicasSettings(); replicas != nil {
		p.intent.ReadReplicasSettings = replicas
	}
	return nil
}

func (p *pendingIndex) normalized(document *protocol.SourceDocument) (*protocol.NormalizedValue, error) {
	owner := proto.CloneOf(p.subject)
	owner.Engine, owner.DisplayName = "ydb", ""
	payload, err := protojson.Marshal(&p.intent)
	if err != nil {
		return nil, err
	}
	model := schema.NativeDesiredModel(schema.IndexSettingsProperties)
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: payload})
	if err != nil {
		return nil, err
	}
	return &protocol.NormalizedValue{Id: "index." + document.GetId() + "." + strconv.Itoa(p.fragment), Value: value, NormalizedOwner: proto.CloneOf(owner),
		CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}, nil
}
