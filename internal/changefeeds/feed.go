// Package changefeeds reads a YDB changefeed as the specification Ptah's
// embedded YDB engine keeps for one, and writes the statements that add, drop
// and change one.
//
// A changefeed is YDB's stream of the changes made to one row table. YDB adds
// it with `ALTER TABLE ... ADD CHANGEFEED <name> WITH (...)` and keeps the
// stream in a topic at `<table>/<name>`, whose retention and consumers change
// in place through `ALTER TOPIC`. No option of the changefeed itself changes in
// place: measured on every YDB line from 25.1.4.7 to 26.2.1.14, `ALTER TABLE
// ... ALTER CHANGEFEED ... SET (MODE = ...)` answers `MODE alter is not
// supported`, and likewise for every other option. A change to one of them
// drops the changefeed and adds it again, and the stream restarts: the records
// nobody read are gone, and every consumer starts over.
//
// Both representations of the provider's changefeed model, the native
// observation inspection reads and the desired intent a declaration decodes
// into, are read into the host's specification (ydbdecl.Changefeed), and the
// comparison, the refusals and the statements work on it. The rules and the
// words are ported from the host's internal/ydbchangefeed and its YDB reader,
// so a changefeed is compared, refused and written as the embedded engine
// compares, refuses and writes it.
package changefeeds

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Feed is one changefeed as the host's specification keeps it, with what only
// a read of the server can say about it.
type Feed struct {
	ydbdecl.Changefeed
	// Disabled is a changefeed YDB reports as disabled. No statement disables
	// one, so a declaration cannot ask for it, and one that is disabled is
	// dropped and added again to enable it.
	Disabled bool
	// Replicated is a stream an asynchronous replication controller created
	// and owns. Ptah neither creates, drops nor changes one.
	Replicated bool
}

// DefaultRetentionSeconds is the retention YDB gives a changefeed's topic when
// the declaration names none: 24 hours, measured on 25.1.4.7 and 26.2.1.14 as
// `retentionPeriod: 86400s`.
const DefaultRetentionSeconds = 24 * 60 * 60

// The modes and formats a changefeed takes, as YQL spells them. YDB has a third
// format, DYNAMODB_STREAMS_JSON, which it writes only for a document table: on
// a row table it answers `DYNAMODB_STREAMS_JSON format incompatible with
// non-document table`, measured on 25.1.4.7 and 26.2.1.14, and Ptah creates no
// document table.
var (
	modes = map[Ydb_Table.ChangefeedMode_Mode]string{
		Ydb_Table.ChangefeedMode_MODE_KEYS_ONLY:          "KEYS_ONLY",
		Ydb_Table.ChangefeedMode_MODE_UPDATES:            "UPDATES",
		Ydb_Table.ChangefeedMode_MODE_NEW_IMAGE:          "NEW_IMAGE",
		Ydb_Table.ChangefeedMode_MODE_OLD_IMAGE:          "OLD_IMAGE",
		Ydb_Table.ChangefeedMode_MODE_NEW_AND_OLD_IMAGES: "NEW_AND_OLD_IMAGES",
	}
	formats = map[Ydb_Table.ChangefeedFormat_Format]string{
		Ydb_Table.ChangefeedFormat_FORMAT_JSON:          "JSON",
		Ydb_Table.ChangefeedFormat_FORMAT_DEBEZIUM_JSON: "DEBEZIUM_JSON",
	}
)

// Modes lists the modes a changefeed takes.
func Modes() []string {
	return []string{"KEYS_ONLY", "UPDATES", "NEW_IMAGE", "OLD_IMAGE", "NEW_AND_OLD_IMAGES"}
}

// Formats lists the formats Ptah writes a changefeed in.
func Formats() []string { return []string{"JSON", "DEBEZIUM_JSON"} }

// replicationAttribute is the attribute an asynchronous replication controller
// writes on the source stream it creates: the replica table's path, the target
// item and whether the replica takes an auto-partitioned topic. It is the one
// attribute the host's reader takes, and it marks a stream Ptah keeps as it is.
const replicationAttribute = "__async_replication"

// consumerServiceType is the attribute YDB gives every consumer it creates
// through ALTER TOPIC, measured on 25.1.4.7 and 26.2.1.14; any other attribute
// is one Ptah does not model.
var consumerServiceType = map[string]string{"_service_type": "data-streams"}

// FromObserved reads one changefeed as the read found it, as the host's reader
// does. The error says why the changefeed holds something Ptah does not model:
// a mode, a format or a state it does not know, an AWS region, attributes other
// than the replication binding, a fraction of a second in an interval, or a
// topic or consumer setting it does not read. The host records such a
// changefeed as not described, so a plan neither drops nor changes it.
//
// The retention is kept where it is not YDB's 24 hours, and a starting
// partition count where it is above one: a changefeed that declared none
// starts with one partition per partition of the table, which a table created
// without settings has one of. An initial scan leaves its trace on the
// changefeed for good, measured on both lines, so the read says whether the
// changefeed asked for one.
func FromObserved(observed *native.ObservedChangefeed) (Feed, error) {
	description := observed.GetDescription()
	if description == nil || observed.GetTopic() == nil {
		return Feed{}, errors.New("the changefeed was read without its description or its topic")
	}
	feed := Feed{Changefeed: ydbdecl.Changefeed{Name: description.GetName(), VirtualTimestamps: description.GetVirtualTimestamps(),
		InitialScan: observed.GetInitialScan(), UserSIDs: description.GetUserSids(), SchemaChanges: description.GetSchemaChanges()}}
	var known bool
	if feed.Mode, known = modes[description.GetMode()]; !known {
		return Feed{}, fmt.Errorf("its mode %s is not one Ptah writes", description.GetMode())
	}
	if feed.Format, known = formats[description.GetFormat()]; !known {
		return Feed{}, fmt.Errorf("its format %s is not one Ptah writes", description.GetFormat())
	}
	if description.GetAwsRegion() != "" {
		return Feed{}, errors.New("it names an AWS region, which Ptah does not model")
	}
	replicated, err := replication(description.GetAttributes())
	if err != nil {
		return Feed{}, err
	}
	feed.Replicated = replicated
	switch description.GetState() {
	case native.ChangefeedDescription_STATE_ENABLED, native.ChangefeedDescription_STATE_INITIAL_SCAN:
	case native.ChangefeedDescription_STATE_DISABLED:
		feed.Disabled = true
	default:
		return Feed{}, fmt.Errorf("its state %s is not one Ptah reads", description.GetState())
	}
	if feed.ResolvedTimestamps, err = interval("its resolved timestamps interval", description.GetResolvedTimestampsInterval()); err != nil {
		return Feed{}, err
	}
	if err := readTopic(&feed.Changefeed, observed.GetTopic()); err != nil {
		return Feed{}, err
	}
	return feed, nil
}

// replication reads a changefeed's attributes: none, or the replication
// binding alone, which marks a stream a replication controller owns.
func replication(attributes map[string]string) (bool, error) {
	if len(attributes) == 0 {
		return false, nil
	}
	raw, found := attributes[replicationAttribute]
	if !found || len(attributes) != 1 {
		return false, errors.New("it holds attributes, which Ptah does not model")
	}
	var binding struct {
		Path             string `json:"path"`
		ID               string `json:"id"`
		Autopartitioning *bool  `json:"supports_topic_autopartitioning"`
	}
	if err := json.Unmarshal([]byte(raw), &binding); err != nil || binding.Autopartitioning == nil || binding.Path == "" || binding.ID == "" {
		return false, errors.New("its replication binding is not one Ptah reads")
	}
	return true, nil
}

// readTopic reads the retention, the partitions and the consumers of a
// changefeed's topic. Its other settings are YDB's own and are not read.
func readTopic(spec *ydbdecl.Changefeed, topic *native.DescribeTopicResult) error {
	retention, err := seconds("its topic's retention period", topic.GetRetentionPeriod())
	if err != nil {
		return err
	}
	if retention != 0 && retention != DefaultRetentionSeconds {
		spec.RetentionPeriod = ydbdecl.FormatInterval(retention)
	}
	partitioning := topic.GetPartitioningSettings()
	if minimum := partitioning.GetMinActivePartitions(); minimum > 1 {
		spec.TopicMinActivePartitions = uint64(minimum)
	}
	if spec.TopicAutoPartitioning, err = autoPartitioned(partitioning.GetAutoPartitioningSettings().GetStrategy()); err != nil {
		return err
	}
	for _, consumer := range topic.GetConsumers() {
		if attributes := consumer.GetAttributes(); len(attributes) > 0 && !maps.Equal(attributes, consumerServiceType) {
			return fmt.Errorf("consumer %q holds attributes, which Ptah does not model", consumer.GetName())
		}
		read, err := topics.ObservedConsumer(consumer)
		if err != nil {
			return fmt.Errorf("consumer %q: %w", consumer.GetName(), err)
		}
		spec.Consumers = append(spec.Consumers, read)
	}
	// By name, as the changefeeds are: the server lists consumers in the order
	// they were added, which a consumer dropped and added again changes.
	slices.SortFunc(spec.Consumers, func(a, b ydbdecl.Consumer) int { return strings.Compare(a.Name, b.Name) })
	return nil
}

// autoPartitioned reads the auto-partitioning of a changefeed's topic, which
// TOPIC_AUTO_PARTITIONING = 'ENABLED' sets to SCALE_UP.
func autoPartitioned(strategy Ydb_Topic.AutoPartitioningStrategy) (bool, error) {
	switch strategy {
	case Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_UNSPECIFIED, Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_DISABLED:
		return false, nil
	case Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_SCALE_UP:
		return true, nil
	default:
		return false, fmt.Errorf("its topic's auto-partitioning strategy %s is not one a changefeed declares", strategy)
	}
}

// FromIntent reads a desired changefeed as the declaration it states. An
// intent converted from a read states the settings of the changefeed's topic
// YDB keeps on its own; they are left out, as the reader leaves them out. A
// setting no statement of a changefeed sets is refused.
func FromIntent(intent *native.ChangefeedIntent) (Feed, error) {
	if intent == nil {
		return Feed{}, errors.New("missing YDB changefeed declaration")
	}
	feed := Feed{Changefeed: ydbdecl.Changefeed{Name: intent.GetName(), VirtualTimestamps: intent.GetVirtualTimestamps(),
		InitialScan: intent.GetInitialScan(), UserSIDs: intent.GetUserSids(), SchemaChanges: intent.GetSchemaChanges()}, Disabled: intent.GetDisabled()}
	var known bool
	if feed.Mode, known = modes[intent.GetMode()]; !known {
		return Feed{}, fmt.Errorf("changefeed %q: its mode %s is none of %s", intent.GetName(), intent.GetMode(), strings.Join(Modes(), ", "))
	}
	if feed.Format, known = formats[intent.GetFormat()]; !known {
		return Feed{}, fmt.Errorf("changefeed %q: its format %s is none of %s", intent.GetName(), intent.GetFormat(), strings.Join(Formats(), ", "))
	}
	if intent.GetAwsRegion() != "" {
		return Feed{}, fmt.Errorf("changefeed %q: the declaration names an AWS region, which no YQL statement sets", intent.GetName())
	}
	replicated, err := replication(intent.GetAttributes())
	if err != nil {
		return Feed{}, fmt.Errorf("changefeed %q: %w", intent.GetName(), err)
	}
	feed.Replicated = replicated
	if feed.ResolvedTimestamps, err = interval("its resolved timestamps interval", intent.GetResolvedTimestampsInterval()); err != nil {
		return Feed{}, fmt.Errorf("changefeed %q: %w", intent.GetName(), err)
	}
	topic := intent.GetTopic()
	retention, err := seconds("its topic's retention period", topic.GetRetentionPeriod())
	if err != nil {
		return Feed{}, fmt.Errorf("changefeed %q: %w", intent.GetName(), err)
	}
	if retention != 0 && retention != DefaultRetentionSeconds {
		feed.RetentionPeriod = ydbdecl.FormatInterval(retention)
	}
	if minimum := topic.GetPartitioningSettings().GetMinActivePartitions(); minimum > 1 {
		feed.TopicMinActivePartitions = uint64(minimum)
	}
	if feed.TopicAutoPartitioning, err = autoPartitioned(topic.GetPartitioningSettings().GetAutoPartitioningSettings().GetStrategy()); err != nil {
		return Feed{}, fmt.Errorf("changefeed %q: %w", intent.GetName(), err)
	}
	for _, consumer := range topic.GetConsumers() {
		if attributes := consumer.GetAttributes(); len(attributes) > 0 && !maps.Equal(attributes, consumerServiceType) {
			return Feed{}, fmt.Errorf("changefeed %q: consumer %q: the declaration states attributes, which no YQL statement sets", intent.GetName(), consumer.GetName())
		}
		declared, err := topics.DeclaredConsumer(consumer)
		if err != nil {
			return Feed{}, fmt.Errorf("changefeed %q: consumer %q: %w", intent.GetName(), consumer.GetName(), err)
		}
		feed.Consumers = append(feed.Consumers, declared)
	}
	return feed, nil
}

// Intent writes a declared changefeed as the provider's desired model: each
// option the declaration names, and zero for each it leaves out. Consumers
// keep their declared order.
func Intent(spec ydbdecl.Changefeed) (*native.ChangefeedIntent, error) {
	mode, err := enumOf(spec.Mode, modes)
	if err != nil {
		return nil, fmt.Errorf("changefeed %q: its mode %q is none of %s", spec.Name, spec.Mode, strings.Join(Modes(), ", "))
	}
	format, err := enumOf(spec.Format, formats)
	if err != nil {
		return nil, fmt.Errorf("changefeed %q: its format %q is none of %s", spec.Name, spec.Format, strings.Join(Formats(), ", "))
	}
	intent := &native.ChangefeedIntent{Name: spec.Name, Mode: mode, Format: format, VirtualTimestamps: spec.VirtualTimestamps,
		InitialScan: spec.InitialScan, UserSids: spec.UserSIDs, SchemaChanges: spec.SchemaChanges}
	if spec.ResolvedTimestamps != "" {
		if intent.ResolvedTimestampsInterval, err = durationOf(spec.ResolvedTimestamps); err != nil {
			return nil, fmt.Errorf("changefeed %q: resolved_timestamps: %w", spec.Name, err)
		}
	}
	// The topic states what the changefeed's options set on it and its
	// consumers; YDB gives it every other setting.
	intent.Topic = &native.TopicIntent{PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: int64(spec.TopicMinActivePartitions)}}
	if spec.TopicAutoPartitioning {
		intent.Topic.PartitioningSettings.AutoPartitioningSettings = &Ydb_Topic.AutoPartitioningSettings{
			Strategy: Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_SCALE_UP}
	}
	if spec.RetentionPeriod != "" {
		if intent.Topic.RetentionPeriod, err = durationOf(spec.RetentionPeriod); err != nil {
			return nil, fmt.Errorf("changefeed %q: retention_period: %w", spec.Name, err)
		}
	}
	for _, consumer := range spec.Consumers {
		declared, err := topics.ConsumerIntent(consumer)
		if err != nil {
			return nil, fmt.Errorf("changefeed %q: %w", spec.Name, err)
		}
		intent.Topic.Consumers = append(intent.Topic.Consumers, declared)
	}
	return intent, nil
}

// enumOf finds the native value YQL spells as written, in any case.
func enumOf[E comparable](written string, names map[E]string) (E, error) {
	for value, name := range names {
		if strings.EqualFold(name, strings.TrimSpace(written)) {
			return value, nil
		}
	}
	var zero E
	return zero, errors.New("unknown value")
}

// durationOf writes an interval a declaration's parse accepted as a duration.
func durationOf(text string) (*durationpb.Duration, error) {
	value, err := ydbdecl.FeedSeconds(text)
	if err != nil {
		return nil, err
	}
	return &durationpb.Duration{Seconds: int64(value)}, nil
}

// seconds reads a duration as whole seconds, zero for none, and refuses a
// negative one or one with a fraction, which no changefeed setting holds.
func seconds(subject string, duration *durationpb.Duration) (uint64, error) {
	if duration == nil {
		return 0, nil
	}
	if duration.GetSeconds() < 0 || duration.GetNanos() != 0 {
		return 0, fmt.Errorf("%s is %s, which is not a whole number of seconds a declaration can hold", subject, duration.AsDuration())
	}
	return uint64(duration.GetSeconds()), nil
}

// interval writes a duration as the interval a declaration takes, empty for
// none.
func interval(subject string, duration *durationpb.Duration) (string, error) {
	value, err := seconds(subject, duration)
	if err != nil || value == 0 {
		return "", err
	}
	return ydbdecl.FormatInterval(value), nil
}
