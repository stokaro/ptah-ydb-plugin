package yqlschema

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// duplicateObject is the host's refusal of an identity two objects take.
func duplicateObject(object Object) error {
	return fmt.Errorf("duplicate feature identity: object %s %s", object.Kind, objectPath(object))
}

// objectPath writes an object's identity as the host writes it in a
// message: its directory, its parent and its name, a component holding a
// dot or a double quote in double quotes.
func objectPath(object Object) string {
	var parts []string
	for _, part := range []string{object.Schema, object.Parent} {
		if part != "" {
			parts = append(parts, displayComponent(part))
		}
	}
	return strings.Join(append(parts, displayComponent(object.Name)), ".")
}

func displayComponent(source string) string {
	if !strings.ContainsAny(source, `."`) {
		return source
	}
	return `"` + strings.ReplaceAll(source, `"`, `""`) + `"`
}

// declare adds object to the statement's document, refusing an identity the
// document already holds.
func (c *converter) declare(object Object) error {
	if c.file.object(object) != nil {
		return duplicateObject(object)
	}
	object.span, object.document = c.statement, c.document
	c.file.Objects = append(c.file.Objects, object)
	return nil
}

func (c *converter) declareTopic(node *createTopic) error {
	if err := ydbdecl.CheckTopic(node.schema, node.name, node.spec); err != nil {
		return err
	}
	object := Object{Kind: KindTopic, Schema: strings.TrimSpace(node.schema), Name: strings.TrimSpace(node.name), Spec: new(node.spec)}
	if c.file.object(object) != nil {
		return fmt.Errorf("topic %s is declared twice", ydbdecl.TopicDisplay(object.Schema, object.Name))
	}
	return c.declare(object)
}

// addConsumer applies `ALTER TOPIC <path> ADD CONSUMER ...` to the topic or
// the changefeed the load declares at the path.
func (c *converter) addConsumer(node *addConsumer) error {
	if _, err := ydbdecl.ParseConsumer(ydbdecl.ConsumerValues(node.consumer)); err != nil {
		return err
	}
	display := ydbdecl.TopicDisplay(node.schema, node.name)
	for _, reading := range c.readings() {
		if topic := reading.object(Object{Kind: KindTopic, Schema: node.schema, Name: node.name}); topic != nil {
			spec := topic.Spec.(*ydbdecl.Topic)
			consumers, err := withConsumer(spec.Consumers, node.consumer, display)
			if err != nil {
				return err
			}
			spec.Consumers = consumers
			return nil
		}
		feeds := slices.Clone(reading.Objects)
		sortObjects(feeds)
		for _, feed := range feeds {
			if feed.Kind != KindChangefeed {
				continue
			}
			directory := feed.Parent
			if feed.Schema != "" {
				directory = feed.Schema + "/" + directory
			}
			if directory != node.schema || feed.Name != node.name {
				continue
			}
			held := reading.object(feed)
			spec := held.Spec.(*ydbdecl.Changefeed)
			consumers, err := withConsumer(spec.Consumers, node.consumer, display)
			if err != nil {
				return err
			}
			spec.Consumers = consumers
			return nil
		}
	}
	return fmt.Errorf("%w: ALTER TOPIC %s ADD CONSUMER names no topic or changefeed this schema declares", errUnmodeled, display)
}

func withConsumer(consumers []ydbdecl.Consumer, consumer ydbdecl.Consumer, topic string) ([]ydbdecl.Consumer, error) {
	if slices.ContainsFunc(consumers, func(held ydbdecl.Consumer) bool { return held.Name == consumer.Name }) {
		return nil, fmt.Errorf("consumer %q is declared twice on topic %s", consumer.Name, topic)
	}
	return append(slices.Clone(consumers), consumer), nil
}

func (c *converter) declareCoordinationNode(node *createCoordinationNode) error {
	object := Object{Kind: KindCoordinationNode, Schema: node.schema, Name: node.name, Spec: new(node.spec)}
	if object.Name == "" {
		return fmt.Errorf("invalid feature value: object identity does not name value kind %q", KindCoordinationNode)
	}
	return c.declare(object)
}

func (c *converter) declareExternalSource(node *createExternalSource) error {
	ref, err := ydbdecl.ExternalPath(node.schema, node.name)
	if err != nil {
		return err
	}
	if err := ydbdecl.ValidateDataSource(node.spec); err != nil {
		return err
	}
	object := Object{Kind: KindExternalSource, Schema: ref.Schema, Name: ref.Name, Spec: new(node.spec)}
	if c.file.object(object) != nil {
		return fmt.Errorf("external data source %s is declared twice", ydbdecl.ExternalDisplay(ref.Schema, ref.Name))
	}
	return c.declare(object)
}

func (c *converter) declareExternalTable(node *createExternalTable) error {
	ref, err := ydbdecl.ExternalPath(node.schema, node.name)
	if err != nil {
		return err
	}
	if err := ydbdecl.ValidateExternalTable(node.spec); err != nil {
		return err
	}
	object := Object{Kind: KindExternalTable, Schema: ref.Schema, Name: ref.Name, Spec: new(node.spec)}
	if c.file.object(object) != nil {
		return fmt.Errorf("external table %s is declared twice", ydbdecl.ExternalDisplay(ref.Schema, ref.Name))
	}
	return c.declare(object)
}
