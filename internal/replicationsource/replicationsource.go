// Package replicationsource decodes the YDB async replications and transfers
// a YAML document or a Go file declares, with the syntax and refusals of the
// host's bundled YDB source (internal/ydbsource): the YAML document keys
// async_replications and transfers, each a mapping of entries keyed by name,
// and the Go directives ptah:schema:async_replication,
// ptah:schema:async_replication:item and ptah:schema:transfer.
package replicationsource

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The YAML document keys this package claims.
const (
	// KeyReplications declares async replications.
	KeyReplications = "async_replications"
	// KeyTransfers declares transfers.
	KeyTransfers = "transfers"
	keyItems     = "items"
	// DirectiveReplication declares one async replication in Go.
	DirectiveReplication = "ptah:schema:async_replication"
	// DirectiveItem declares one item of a replication the same Go file
	// declares.
	DirectiveItem = "ptah:schema:async_replication:item"
	// DirectiveTransfer declares one transfer in Go.
	DirectiveTransfer = "ptah:schema:transfer"
)

// GoDirectives declares the Go directives of both families, with the
// attributes of the host's own directives of the same names.
func GoDirectives() []*protocol.GoSourceDirective {
	return []*protocol.GoSourceDirective{
		{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectiveReplication, Selector: DirectiveReplication,
			Attributes: replicationKeys(), Models: []string{schema.ReplicationProperties}},
		{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectiveItem, Selector: DirectiveItem,
			Attributes: []string{replication.AttributeReplication, replication.AttributeSchema, replication.AttributeSource, replication.AttributeTarget},
			Models:     []string{schema.ReplicationProperties}},
		{Scope: protocol.GoSourceDirective_DOCUMENT, Directive: DirectiveTransfer, Selector: DirectiveTransfer,
			Attributes: transferKeys(), Models: []string{schema.TransferProperties}},
	}
}

// YAMLKeys declares both document keys.
func YAMLKeys() []*protocol.YAMLSourceKey {
	return []*protocol.YAMLSourceKey{
		{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: KeyReplications, Selector: KeyReplications, Models: []string{schema.ReplicationProperties}},
		{Scope: protocol.YAMLSourceKey_DOCUMENT, Key: KeyTransfers, Selector: KeyTransfers, Models: []string{schema.TransferProperties}},
	}
}

// Owns reports whether a fragment carries one of this package's claims.
func Owns(fragment *protocol.SourceFragment) bool {
	if key := fragment.GetYamlKey(); key != nil {
		return key.GetScope() == protocol.YAMLSourceKey_DOCUMENT && (key.GetKey() == KeyReplications || key.GetKey() == KeyTransfers)
	}
	switch fragment.GetGoDirective().GetDirective() {
	case DirectiveReplication, DirectiveItem, DirectiveTransfer:
		return true
	}
	return false
}

// connectionKeys are the keys a replication and a transfer spell their
// connection with.
func connectionKeys() []string {
	return []string{replication.AttributeConnectionString, replication.AttributeTokenSecretName, replication.AttributeTokenSecretPath,
		replication.AttributeUser, replication.AttributePasswordSecretName, replication.AttributePasswordSecretPath}
}

// replicationKeys are the scalar keys of a replication entry.
func replicationKeys() []string {
	return append([]string{replication.AttributeName, replication.AttributeSchema, replication.AttributeConsistencyLevel, replication.AttributeCommitInterval}, connectionKeys()...)
}

// transferKeys are the scalar keys of a transfer entry.
func transferKeys() []string {
	return append([]string{replication.AttributeName, replication.AttributeSchema, replication.AttributeSource, replication.AttributeTarget,
		replication.AttributeUsing, replication.AttributeConsumer, replication.AttributeBatchSizeBytes, replication.AttributeFlushInterval}, connectionKeys()...)
}

// Decode reads every replication and transfer a YAML document declares and
// returns the values of each fragment that declares them, by index. An invalid
// declaration refuses the whole document.
func Decode(ctx context.Context, document *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error) {
	if document.GetFormat() == protocol.SourceDocument_GO_ANNOTATIONS {
		return decodeGo(ctx, document)
	}
	result := make(map[int][]*protocol.SourceModelValue)
	seen := make(map[string]bool)
	position := 0
	for index, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) {
			continue
		}
		if document.Format != protocol.SourceDocument_YAML {
			return nil, fmt.Errorf("unsupported YDB replication source format")
		}
		if fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
			return nil, fmt.Errorf("unsupported YDB replication source attachment")
		}
		mapping := fragment.GetValue().GetMapping()
		if mapping == nil {
			return nil, fmt.Errorf("%s requires a mapping", fragment.Selector)
		}
		result[index] = nil
		entries := slices.Clone(mapping.GetEntries())
		slices.SortFunc(entries, func(a, b *protocol.SourceEntry) int { return strings.Compare(a.Key, b.Key) })
		for _, entry := range entries {
			var value *protocol.Envelope
			var family, display string
			var err error
			switch fragment.Selector {
			case KeyReplications:
				family = "async replication"
				value, display, err = replicationEntry(entry)
			case KeyTransfers:
				family = "transfer"
				value, display, err = transferEntry(entry)
			default:
				return nil, fmt.Errorf("unsupported YDB replication source key")
			}
			if err != nil {
				return nil, err
			}
			if key := value.Kind + "\x00" + display; seen[key] {
				return nil, fmt.Errorf("%s %q: %s %s is declared twice", family, entry.Key, family, display)
			}
			seen[value.Kind+"\x00"+display] = true
			result[index] = append(result[index], &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
				Value: &protocol.NormalizedValue{Id: "replication." + strconv.Itoa(position), Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
					CanonicalFormat: schema.NativeDesiredModel(value.Kind).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)},
				Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: fragment.Id, Root: protocol.SourceTrace_CONTENT,
					Path: []*protocol.SourcePathStep{{Step: &protocol.SourcePathStep_Key{Key: entry.Key}}}}})
			position++
		}
	}
	return result, nil
}

// replicationEntry reads one entry of async_replications, as the host reads
// it: the connection and consistency through the declaration rules, then
// each item in the order written.
func replicationEntry(entry *protocol.SourceEntry) (*protocol.Envelope, string, error) {
	values, items, err := fields(entry, replicationKeys(), true)
	if err != nil {
		return nil, "", fmt.Errorf("async replication %q: %w", entry.Key, err)
	}
	spec, err := replication.ParseReplication(values)
	if err != nil {
		return nil, "", fmt.Errorf("async replication %q: %w", entry.Key, err)
	}
	if len(items) == 0 {
		return nil, "", fmt.Errorf("async replication %q: declares no item; list the tables it replicates under items", entry.Key)
	}
	for position, written := range items {
		item, err := replication.ParseItem(written)
		if err != nil {
			return nil, "", fmt.Errorf("async replication %q, item %d: %w", entry.Key, position+1, err)
		}
		spec.Items = append(spec.Items, item)
	}
	owner, display, err := declaredOwner(schema.ReplicationProperties, values, entry.Key)
	if err != nil {
		return nil, "", fmt.Errorf("async replication %q: %w", entry.Key, err)
	}
	if err := replication.ValidateReplication(spec); err != nil {
		return nil, "", fmt.Errorf("async replication %q: %w", entry.Key, err)
	}
	value, err := schema.CaptureReplicationIntent(owner, spec)
	if err != nil {
		return nil, "", fmt.Errorf("async replication %q: %w", entry.Key, err)
	}
	return value, display, nil
}

// transferEntry reads one entry of transfers.
func transferEntry(entry *protocol.SourceEntry) (*protocol.Envelope, string, error) {
	values, _, err := fields(entry, transferKeys(), false)
	if err != nil {
		return nil, "", fmt.Errorf("transfer %q: %w", entry.Key, err)
	}
	// The host reads source, target and using as present whether written
	// or not, so a missing one is refused as an empty one.
	for _, key := range []string{replication.AttributeSource, replication.AttributeTarget, replication.AttributeUsing} {
		if _, ok := values[key]; !ok {
			values[key] = ""
		}
	}
	spec, err := replication.ParseTransfer(values)
	if err != nil {
		return nil, "", fmt.Errorf("transfer %q: %w", entry.Key, err)
	}
	owner, display, err := declaredOwner(schema.TransferProperties, values, entry.Key)
	if err != nil {
		return nil, "", fmt.Errorf("transfer %q: %w", entry.Key, err)
	}
	value, err := schema.CaptureTransferIntent(owner, spec)
	if err != nil {
		return nil, "", fmt.Errorf("transfer %q: %w", entry.Key, err)
	}
	return value, display, nil
}

// fields reads an entry's scalar keys, and the items of a replication where
// withItems is set. A key the family does not name is refused.
func fields(entry *protocol.SourceEntry, keys []string, withItems bool) (map[string]string, []map[string]string, error) {
	mapping := entry.GetValue().GetMapping()
	if mapping == nil {
		return nil, nil, fmt.Errorf("requires a mapping")
	}
	values := make(map[string]string)
	var items []map[string]string
	for _, field := range mapping.GetEntries() {
		switch {
		case withItems && field.Key == keyItems:
			sequence := field.GetValue().GetSequence()
			if sequence == nil && field.GetValue().GetNull() == nil {
				return nil, nil, fmt.Errorf("%s requires a sequence", keyItems)
			}
			for position, value := range sequence.GetValues() {
				item, err := itemFields(value)
				if err != nil {
					return nil, nil, fmt.Errorf("item %d: %w", position+1, err)
				}
				items = append(items, item)
			}
		case slices.Contains(keys, field.Key):
			text, err := scalar(field.GetValue())
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", field.Key, err)
			}
			values[field.Key] = text
		default:
			return nil, nil, fmt.Errorf("field %s not found", field.Key)
		}
	}
	return values, items, nil
}

// itemFields reads one item of a replication: its source and its target.
func itemFields(value *protocol.SourceValue) (map[string]string, error) {
	mapping := value.GetMapping()
	if mapping == nil {
		return nil, fmt.Errorf("an item requires a mapping")
	}
	item := map[string]string{replication.AttributeSource: "", replication.AttributeTarget: ""}
	for _, field := range mapping.GetEntries() {
		if field.Key != replication.AttributeSource && field.Key != replication.AttributeTarget {
			return nil, fmt.Errorf("field %s not found", field.Key)
		}
		text, err := scalar(field.GetValue())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field.Key, err)
		}
		item[field.Key] = text
	}
	return item, nil
}

// declaredOwner reads a declaration's directory and name, the entry's key
// where it names none, and refuses a path an object cannot take.
func declaredOwner(kind string, values map[string]string, key string) (*protocol.Reference, string, error) {
	name := strings.TrimSpace(values[replication.AttributeName])
	if name == "" {
		name = key
	}
	directory := strings.Trim(strings.TrimSpace(values[replication.AttributeSchema]), "/")
	switch {
	case name == "":
		return nil, "", &replication.DeclarationError{Attribute: replication.AttributeName, Reason: "an async replication or a transfer needs a name"}
	case strings.Contains(name, "/"):
		return nil, "", &replication.DeclarationError{Attribute: replication.AttributeName, Value: name,
			Reason: "holds a slash; name the directory with " + replication.AttributeSchema}
	case schema.ValidateNativeOwner(&protocol.Reference{Engine: "ydb", Kind: kind, Name: name}) != nil:
		return nil, "", &replication.DeclarationError{Attribute: replication.AttributeName, Value: name, Reason: "is not a path segment"}
	}
	owner := &protocol.Reference{Engine: "ydb", Kind: kind, Schema: directory, Name: name}
	if directory != "" && schema.ValidateNativeOwner(owner) != nil {
		return nil, "", &replication.DeclarationError{Attribute: replication.AttributeSchema, Value: directory,
			Reason: "is not a directory path relative to the database root"}
	}
	display := name
	if directory != "" {
		display = directory + "/" + name
	}
	return owner, display, nil
}

// scalar reads a YAML scalar as the text the host's bundled source reads it
// as: a number keeps its digits, a boolean reads true or false, and a null
// is a value written empty, which the setting's parse refuses.
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

// pendingReplication is a replication a Go file declares, before the file's
// items join it.
type pendingReplication struct {
	fragment int
	values   map[string]string
	spec     replication.ReplicationSpec
	schema   string
	name     string
}

// decodeGo reads the replications, their items and the transfers a Go file
// declares, as the host's bundled source reads them: each item joins the
// replication of its name and directory in the same file, and a replication
// with no item is refused.
func decodeGo(ctx context.Context, document *protocol.SourceDocument) (map[int][]*protocol.SourceModelValue, error) {
	result := make(map[int][]*protocol.SourceModelValue)
	var replications []pendingReplication
	var items []pendingItem
	var transfers []pendingTransfer
	for index, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) {
			continue
		}
		if fragment.BindingId != "" || fragment.Subject != nil || len(fragment.GetInheritedAttributes().GetEntries()) != 0 {
			return nil, fmt.Errorf("unsupported YDB replication source attachment")
		}
		result[index] = nil
		values, err := attributes(fragment.GetValue().GetMapping(), fragment.Selector)
		if err != nil {
			return nil, err
		}
		switch fragment.Selector {
		case DirectiveReplication:
			spec, err := replication.ParseReplication(values)
			if err != nil {
				return nil, err
			}
			replications = append(replications, pendingReplication{fragment: index, values: values, spec: spec,
				schema: strings.Trim(strings.TrimSpace(values[replication.AttributeSchema]), "/"), name: strings.TrimSpace(values[replication.AttributeName])})
		case DirectiveItem:
			item, err := replication.ParseItem(values)
			if err != nil {
				return nil, err
			}
			items = append(items, pendingItem{schema: strings.Trim(strings.TrimSpace(values[replication.AttributeSchema]), "/"),
				replication: strings.TrimSpace(values[replication.AttributeReplication]), item: item})
		case DirectiveTransfer:
			transfers = append(transfers, pendingTransfer{fragment: index, values: values})
		default:
			return nil, fmt.Errorf("unsupported YDB replication source directive")
		}
	}
	if err := attachItems(replications, items); err != nil {
		return nil, err
	}
	if err := addGoValues(document, result, replications, transfers); err != nil {
		return nil, err
	}
	return result, nil
}

// pendingItem is an item directive held until every replication of the file
// is known, since the replication it joins may come later in the file.
type pendingItem struct {
	schema, replication string
	item                replication.Item
}

// pendingTransfer is a transfer directive, parsed once the replications of the
// file are captured.
type pendingTransfer struct {
	fragment int
	values   map[string]string
}

// attachItems adds each held item to the replication of its name and
// directory, in declaration order. An item of a replication the file does not
// declare, or a second item with one target, refuses the file.
func attachItems(replications []pendingReplication, items []pendingItem) error {
	for _, pending := range items {
		index := slices.IndexFunc(replications, func(declared pendingReplication) bool {
			return declared.name == pending.replication && declared.schema == pending.schema
		})
		if index < 0 {
			return fmt.Errorf("the file declares no async replication %q for the item replicating %q", display(pending.schema, pending.replication), pending.item.Source)
		}
		declared := &replications[index]
		if slices.ContainsFunc(declared.spec.Items, func(have replication.Item) bool {
			return strings.Trim(have.Target, "/") == strings.Trim(pending.item.Target, "/")
		}) {
			return fmt.Errorf("async replication %q declares two items with target %q", display(declared.schema, declared.name), pending.item.Target)
		}
		declared.spec.Items = append(declared.spec.Items, pending.item)
	}
	return nil
}

// addGoValues captures the replications, then the transfers, of a Go file and
// appends each value to the values of its fragment in result, numbering them
// in that order. A replication with no item, or a second declaration of one
// kind and path, refuses the file.
func addGoValues(document *protocol.SourceDocument, result map[int][]*protocol.SourceModelValue, replications []pendingReplication, transfers []pendingTransfer) error {
	seen := make(map[string]bool)
	position := 0
	add := func(fragment int, value *protocol.Envelope, family, path string) error {
		if key := value.Kind + "\x00" + path; seen[key] {
			return fmt.Errorf("%s %s is declared twice", family, path)
		}
		seen[value.Kind+"\x00"+path] = true
		result[fragment] = append(result[fragment], &protocol.SourceModelValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
			Value: &protocol.NormalizedValue{Id: "replication." + strconv.Itoa(position), Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
				CanonicalFormat: schema.NativeDesiredModel(value.Kind).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)},
			Source: &protocol.SourceTrace{DocumentId: document.Id, FragmentId: document.Fragments[fragment].Id, Root: protocol.SourceTrace_CONTENT}})
		position++
		return nil
	}
	for _, declared := range replications {
		if len(declared.spec.Items) == 0 {
			return fmt.Errorf("async replication %q declares no item; declare the tables it replicates with //%s in the same file", display(declared.schema, declared.name), DirectiveItem)
		}
		owner, path, err := declaredOwner(schema.ReplicationProperties, declared.values, "")
		if err != nil {
			return err
		}
		if err := replication.ValidateReplication(declared.spec); err != nil {
			return err
		}
		value, err := schema.CaptureReplicationIntent(owner, declared.spec)
		if err != nil {
			return err
		}
		if err := add(declared.fragment, value, "async replication", path); err != nil {
			return err
		}
	}
	for _, declared := range transfers {
		spec, err := replication.ParseTransfer(declared.values)
		if err != nil {
			return err
		}
		owner, path, err := declaredOwner(schema.TransferProperties, declared.values, "")
		if err != nil {
			return err
		}
		value, err := schema.CaptureTransferIntent(owner, spec)
		if err != nil {
			return err
		}
		if err := add(declared.fragment, value, "transfer", path); err != nil {
			return err
		}
	}
	return nil
}

// attributes reads a Go directive's attributes, which the host lexed as
// text.
func attributes(mapping *protocol.SourceMapping, directive string) (map[string]string, error) {
	if mapping == nil {
		return nil, fmt.Errorf("%s requires attributes", directive)
	}
	values := make(map[string]string)
	for _, entry := range mapping.GetEntries() {
		text, ok := entry.GetValue().GetValue().(*protocol.SourceValue_Text)
		if !ok {
			return nil, fmt.Errorf("attribute %s requires text", entry.Key)
		}
		values[entry.Key] = text.Text
	}
	return values, nil
}

// display names an object by its path relative to the database root, for
// messages.
func display(directory, name string) string {
	if directory == "" {
		return name
	}
	return directory + "/" + name
}
