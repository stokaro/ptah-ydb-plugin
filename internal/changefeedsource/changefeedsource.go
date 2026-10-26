// Package changefeedsource decodes the changefeeds a YAML document or a Go file
// declares for its tables, with the syntax and refusals of the host's bundled
// YDB source (internal/ydbsource): the YAML table key changefeeds, and the Go
// directives ptah:schema:changefeed and ptah:schema:changefeed:consumer.
//
// The changefeeds of one table become one desired facet of the table, the
// provider's changefeed set model. A table a source declares with no
// changefeed gets no facet, and the model's source formats tell the host that
// such a table is declared to have none.
package changefeedsource

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The syntax this package claims.
const (
	// KeyChangefeeds is the YAML table key that holds a mapping of changefeed
	// names to their options.
	KeyChangefeeds = "changefeeds"
	// DirectiveChangefeed declares one changefeed of the struct's table, or
	// of the table its table attribute names.
	DirectiveChangefeed = "ptah:schema:changefeed"
	// DirectiveConsumer declares one consumer of a changefeed's topic.
	DirectiveConsumer = "ptah:schema:changefeed:consumer"

	attributeTable      = "table"
	attributeChangefeed = "changefeed"
	keyConsumers        = "consumers"
)

// options are the options a changefeed declares, in the host's names.
func options() []string {
	return []string{ydbdecl.AttributeMode, ydbdecl.AttributeFormat, ydbdecl.AttributeVirtualTimestamps, ydbdecl.AttributeResolvedTimestamps,
		ydbdecl.AttributeInitialScan, ydbdecl.AttributeUserSIDs, ydbdecl.AttributeSchemaChanges, ydbdecl.AttributeTopicMinActivePartitions,
		ydbdecl.AttributeTopicAutoPartitioning, ydbdecl.AttributeFeedRetentionPeriod}
}

// consumerSettings are the settings a consumer declares.
func consumerSettings() []string {
	return []string{ydbdecl.AttributeImportant, ydbdecl.AttributeReadFrom, ydbdecl.AttributeSupportedCodecs, ydbdecl.AttributeAvailabilityPeriod}
}

// YAMLKey declares the YAML table key changefeeds.
func YAMLKey() *protocol.YAMLSourceKey {
	return &protocol.YAMLSourceKey{Scope: protocol.YAMLSourceKey_TABLE, Key: KeyChangefeeds, Selector: KeyChangefeeds, Models: []string{schema.ChangefeedProperties}}
}

// GoDirectives declares the Go directives of a changefeed and of a consumer
// of its topic, with the attributes of the host's own directives of the same
// names.
func GoDirectives() []*protocol.GoSourceDirective {
	return []*protocol.GoSourceDirective{
		{Scope: protocol.GoSourceDirective_TABLE, Directive: DirectiveChangefeed, Selector: DirectiveChangefeed,
			Attributes: append([]string{ydbdecl.ChangefeedName, attributeTable}, options()...), Models: []string{schema.ChangefeedProperties}},
		{Scope: protocol.GoSourceDirective_TABLE, Directive: DirectiveConsumer, Selector: DirectiveConsumer,
			Attributes: append([]string{ydbdecl.TopicName, attributeChangefeed, attributeTable}, consumerSettings()...), Models: []string{schema.ChangefeedProperties}},
	}
}

// Owns reports whether a fragment carries one of this package's claims.
func Owns(fragment *protocol.SourceFragment) bool {
	if key := fragment.GetYamlKey(); key != nil {
		return key.GetScope() == protocol.YAMLSourceKey_TABLE && key.GetKey() == KeyChangefeeds
	}
	if directive := fragment.GetGoDirective(); directive != nil {
		return directive.GetScope() == protocol.GoSourceDirective_TABLE &&
			(directive.GetDirective() == DirectiveChangefeed || directive.GetDirective() == DirectiveConsumer)
	}
	return false
}

// Table is the changefeed facet a document declares for one table.
type Table struct {
	// Binding is the common table the facet belongs to.
	Binding *protocol.SourceBinding
	// Fragment is the index in the document of the first fragment that
	// declared a changefeed of the table. The facet is returned with it.
	Fragment int
	// Value is the normalized desired facet.
	Value *protocol.NormalizedValue
}

// declaredTable collects the changefeeds of one table in declaration order.
type declaredTable struct {
	binding  *protocol.SourceBinding
	fragment int
	feeds    []ydbdecl.Changefeed
}

// Decode reads every changefeed a document declares and returns one facet per
// table that declares any, in the order of each table's first declaring
// fragment. It reads no file and no environment, and any invalid declaration
// refuses the whole document. A consumer of a changefeed the file does not
// declare for its table is refused rather than dropped, as the host refuses it:
// a reader nobody creates is a declaration with no effect.
func Decode(ctx context.Context, document *protocol.SourceDocument) ([]Table, error) {
	var order []string
	tables := make(map[string]*declaredTable)
	type pendingConsumer struct {
		binding    *protocol.SourceBinding
		changefeed string
		consumer   ydbdecl.Consumer
	}
	var consumers []pendingConsumer
	tableOf := func(binding *protocol.SourceBinding, fragment int) *declaredTable {
		target, seen := tables[binding.GetId()]
		if !seen {
			target = &declaredTable{binding: binding, fragment: fragment}
			tables[binding.GetId()] = target
			order = append(order, binding.GetId())
		}
		return target
	}
	for index, fragment := range document.GetFragments() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !Owns(fragment) {
			continue
		}
		mapping := fragment.GetValue().GetMapping()
		switch {
		case document.GetFormat() == protocol.SourceDocument_YAML && fragment.GetSelector() == KeyChangefeeds:
			binding, err := tablesource.Binding(document, fragment, "a changefeed")
			if err != nil {
				return nil, fmt.Errorf("fragment %q: %w", fragment.GetId(), err)
			}
			label := tableLabel(binding.GetObject())
			if mapping == nil {
				if fragment.GetValue().GetNull() != nil {
					continue
				}
				return nil, fmt.Errorf("table %q: %s requires a mapping", label, KeyChangefeeds)
			}
			target := tableOf(binding, index)
			for _, entry := range mapping.GetEntries() {
				feed, err := yamlChangefeed(entry)
				if err != nil {
					return nil, fmt.Errorf("table %q: changefeed %q: %w", label, entry.GetKey(), err)
				}
				if err := target.add(feed); err != nil {
					return nil, err
				}
			}
		case document.GetFormat() == protocol.SourceDocument_GO_ANNOTATIONS && fragment.GetSelector() == DirectiveChangefeed:
			values, err := attributes(mapping)
			if err != nil {
				return nil, err
			}
			feed, err := ydbdecl.ParseChangefeed(values)
			if err != nil {
				return nil, err
			}
			binding, err := tablesource.Binding(document, fragment, "a changefeed")
			if err != nil {
				return nil, err
			}
			if err := tableOf(binding, index).add(feed); err != nil {
				return nil, err
			}
		case document.GetFormat() == protocol.SourceDocument_GO_ANNOTATIONS && fragment.GetSelector() == DirectiveConsumer:
			values, err := attributes(mapping)
			if err != nil {
				return nil, err
			}
			consumer, err := ydbdecl.ParseConsumer(values)
			if err != nil {
				return nil, err
			}
			changefeed := strings.TrimSpace(values[attributeChangefeed])
			if changefeed == "" {
				return nil, fmt.Errorf("%s needs the changefeed whose topic it reads", DirectiveConsumer)
			}
			binding, err := tablesource.Binding(document, fragment, "a changefeed")
			if err != nil {
				return nil, err
			}
			consumers = append(consumers, pendingConsumer{binding: binding, changefeed: changefeed, consumer: consumer})
		default:
			return nil, fmt.Errorf("unsupported YDB changefeed source format")
		}
	}
	for _, pending := range consumers {
		target := tables[pending.binding.GetId()]
		label := tableLabel(pending.binding.GetObject())
		index := -1
		if target != nil {
			index = slices.IndexFunc(target.feeds, func(feed ydbdecl.Changefeed) bool { return feed.Name == pending.changefeed })
		}
		if index < 0 {
			return nil, fmt.Errorf("table %q declares no changefeed %q for consumer %q", label, pending.changefeed, pending.consumer.Name)
		}
		feed := &target.feeds[index]
		if slices.ContainsFunc(feed.Consumers, func(have ydbdecl.Consumer) bool { return have.Name == pending.consumer.Name }) {
			return nil, fmt.Errorf("table %q: changefeed %q declares consumer %q twice", label, feed.Name, pending.consumer.Name)
		}
		feed.Consumers = append(feed.Consumers, pending.consumer)
	}
	var result []Table
	for _, id := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target := tables[id]
		if len(target.feeds) == 0 {
			continue
		}
		value, err := target.facet(document)
		if err != nil {
			return nil, fmt.Errorf("table %q: %w", tableLabel(target.binding.GetObject()), err)
		}
		result = append(result, Table{Binding: target.binding, Fragment: target.fragment, Value: value})
	}
	return result, nil
}

// add declares one changefeed of the table, refusing a second of one name.
func (t *declaredTable) add(feed ydbdecl.Changefeed) error {
	if slices.ContainsFunc(t.feeds, func(have ydbdecl.Changefeed) bool { return have.Name == feed.Name }) {
		return fmt.Errorf("table %q declares changefeed %q twice", tableLabel(t.binding.GetObject()), feed.Name)
	}
	t.feeds = append(t.feeds, feed)
	return nil
}

// facet writes the table's changefeeds as the provider's desired facet.
func (t *declaredTable) facet(document *protocol.SourceDocument) (*protocol.NormalizedValue, error) {
	set := &native.ChangefeedSetIntent{}
	for _, feed := range t.feeds {
		intent, err := changefeeds.Intent(feed)
		if err != nil {
			return nil, err
		}
		set.Changefeeds = append(set.Changefeeds, intent)
	}
	payload, err := protojson.Marshal(set)
	if err != nil {
		return nil, err
	}
	owner := proto.CloneOf(t.binding.GetObject())
	model := schema.NativeDesiredModel(schema.ChangefeedProperties)
	value, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: payload})
	if err != nil {
		return nil, err
	}
	return &protocol.NormalizedValue{Id: "changefeeds." + document.GetId() + "." + strconv.Itoa(t.fragment), Value: value, NormalizedOwner: proto.CloneOf(owner),
		CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}, nil
}

// yamlChangefeed reads one entry of a table's changefeeds mapping: the
// changefeed keyed by its name, with each option keyed as the annotation keys
// it and its consumers keyed by name.
func yamlChangefeed(entry *protocol.SourceEntry) (ydbdecl.Changefeed, error) {
	values := map[string]string{}
	var consumerEntries []*protocol.SourceEntry
	if entry.GetValue().GetNull() == nil {
		fields := entry.GetValue().GetMapping()
		if fields == nil {
			return ydbdecl.Changefeed{}, fmt.Errorf("a changefeed requires a mapping")
		}
		for _, field := range fields.GetEntries() {
			switch {
			case field.GetKey() == keyConsumers:
				consumerMapping := field.GetValue().GetMapping()
				if consumerMapping == nil && field.GetValue().GetNull() == nil {
					return ydbdecl.Changefeed{}, fmt.Errorf("%s requires a mapping", keyConsumers)
				}
				consumerEntries = consumerMapping.GetEntries()
			case slices.Contains(options(), field.GetKey()):
				text, err := scalar(field.GetValue())
				if err != nil {
					return ydbdecl.Changefeed{}, fmt.Errorf("%s: %w", field.GetKey(), err)
				}
				values[field.GetKey()] = text
			default:
				return ydbdecl.Changefeed{}, fmt.Errorf("unknown key %q", field.GetKey())
			}
		}
	}
	values[ydbdecl.ChangefeedName] = entry.GetKey()
	feed, err := ydbdecl.ParseChangefeed(values)
	if err != nil {
		return ydbdecl.Changefeed{}, err
	}
	for _, consumerEntry := range consumerEntries {
		consumer, err := yamlConsumer(consumerEntry)
		if err != nil {
			return ydbdecl.Changefeed{}, fmt.Errorf("consumer %q: %w", consumerEntry.GetKey(), err)
		}
		feed.Consumers = append(feed.Consumers, consumer)
	}
	return feed, nil
}

// yamlConsumer reads one consumer of a changefeed's topic, keyed by its name.
func yamlConsumer(entry *protocol.SourceEntry) (ydbdecl.Consumer, error) {
	values := map[string]string{}
	if entry.GetValue().GetNull() == nil {
		fields := entry.GetValue().GetMapping()
		if fields == nil {
			return ydbdecl.Consumer{}, fmt.Errorf("a consumer requires a mapping")
		}
		for _, field := range fields.GetEntries() {
			switch {
			case field.GetKey() == ydbdecl.AttributeSupportedCodecs:
				list, err := codecList(field.GetValue())
				if err != nil {
					return ydbdecl.Consumer{}, fmt.Errorf("%s: %w", field.GetKey(), err)
				}
				values[field.GetKey()] = list
			case slices.Contains(consumerSettings(), field.GetKey()):
				text, err := scalar(field.GetValue())
				if err != nil {
					return ydbdecl.Consumer{}, fmt.Errorf("%s: %w", field.GetKey(), err)
				}
				values[field.GetKey()] = text
			default:
				return ydbdecl.Consumer{}, fmt.Errorf("unknown key %q", field.GetKey())
			}
		}
	}
	values[ydbdecl.TopicName] = entry.GetKey()
	return ydbdecl.ParseConsumer(values)
}

// attributes reads a Go directive's attributes, which the host lexed as text.
func attributes(mapping *protocol.SourceMapping) (map[string]string, error) {
	if mapping == nil {
		return nil, fmt.Errorf("a changefeed directive requires attributes")
	}
	values := map[string]string{}
	for _, entry := range mapping.GetEntries() {
		text, ok := entry.GetValue().GetValue().(*protocol.SourceValue_Text)
		if !ok {
			return nil, fmt.Errorf("attribute %s requires text", entry.GetKey())
		}
		values[entry.GetKey()] = text.Text
	}
	return values, nil
}

// scalar reads a YAML scalar as the text the host's bundled source reads it
// as: a number keeps its digits, a boolean reads true or false, and a null is
// a value written empty, which the setting's parse refuses.
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

// codecList reads a codec list written as a sequence or as one
// comma-separated scalar, as the host reads one.
func codecList(value *protocol.SourceValue) (string, error) {
	sequence := value.GetSequence()
	if sequence == nil {
		return scalar(value)
	}
	var names []string
	for _, item := range sequence.GetValues() {
		text, err := scalar(item)
		if err != nil {
			return "", err
		}
		names = append(names, text)
	}
	return strings.Join(names, ","), nil
}

// tableLabel names a table as the host's refusals name it.
func tableLabel(table *protocol.Reference) string {
	if table.GetSchema() == "" {
		return table.GetName()
	}
	return table.GetSchema() + "." + table.GetName()
}
