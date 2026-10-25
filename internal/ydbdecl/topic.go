package ydbdecl

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
)

// The attributes that declare a topic.
const (
	TopicName                       = "name"
	TopicSchema                     = "schema"
	AttributeMinActivePartitions    = "min_active_partitions"
	AttributeMaxActivePartitions    = "max_active_partitions"
	AttributeStrategy               = "auto_partitioning_strategy"
	AttributeUpUtilizationPercent   = "auto_partitioning_up_utilization_percent"
	AttributeDownUtilizationPercent = "auto_partitioning_down_utilization_percent"
	AttributeStabilizationWindow    = "auto_partitioning_stabilization_window"
	AttributeRetentionPeriod        = "retention_period"
	AttributeWriteSpeed             = "partition_write_speed_bytes_per_second"
	AttributeWriteBurst             = "partition_write_burst_bytes"
	AttributeSupportedCodecs        = "supported_codecs"
)

// The attributes that declare a consumer.
const (
	AttributeImportant          = "important"
	AttributeReadFrom           = "read_from"
	AttributeAvailabilityPeriod = "availability_period"
)

// The auto-partitioning strategies a topic takes.
const (
	StrategyDisabled       = "disabled"
	StrategyScaleUp        = "scale_up"
	StrategyScaleUpAndDown = "scale_up_and_down"
	StrategyPaused         = "paused"
)

func strategies() []string {
	return []string{StrategyDisabled, StrategyScaleUp, StrategyScaleUpAndDown, StrategyPaused}
}

const defaultMinActivePartitions = 1

// CodecNames lists the codecs a topic and a consumer take, in the order a
// refusal lists them.
func CodecNames() []string { return []string{"raw", "gzip", "lzop", "zstd", "custom"} }

// Topic is a standalone topic's settings and consumers, as the host's topic
// spec encodes them.
type Topic struct {
	MinActivePartitions                    uint64     `json:"min_active_partitions,omitempty"`
	MaxActivePartitions                    uint64     `json:"max_active_partitions,omitempty"`
	AutoPartitioningStrategy               string     `json:"auto_partitioning_strategy,omitempty"`
	AutoPartitioningUpUtilizationPercent   uint32     `json:"auto_partitioning_up_utilization_percent,omitempty"`
	AutoPartitioningDownUtilizationPercent uint32     `json:"auto_partitioning_down_utilization_percent,omitempty"`
	AutoPartitioningStabilizationWindow    string     `json:"auto_partitioning_stabilization_window,omitempty"`
	RetentionPeriod                        string     `json:"retention_period,omitempty"`
	PartitionWriteSpeedBytesPerSecond      uint64     `json:"partition_write_speed_bytes_per_second,omitempty"`
	PartitionWriteBurstBytes               uint64     `json:"partition_write_burst_bytes,omitempty"`
	SupportedCodecs                        []string   `json:"supported_codecs,omitempty"`
	Consumers                              []Consumer `json:"consumers,omitempty"`
}

// Consumer is one consumer of a topic or of a changefeed's topic.
type Consumer struct {
	Name               string   `json:"name"`
	Important          bool     `json:"important,omitempty"`
	ReadFrom           string   `json:"read_from,omitempty"`
	SupportedCodecs    []string `json:"supported_codecs,omitempty"`
	AvailabilityPeriod string   `json:"availability_period,omitempty"`
}

// TopicError is an attribute whose value a topic or a consumer cannot carry.
type TopicError struct {
	Attribute string
	Value     string
	Reason    string
}

func (e *TopicError) Error() string {
	if e.Value == "" {
		return fmt.Sprintf("invalid %s: %s", e.Attribute, e.Reason)
	}
	return fmt.Sprintf("invalid %s %q: %s", e.Attribute, e.Value, e.Reason)
}

// ParseTopic reads a topic's settings out of values, keyed by attribute name.
// It reads no consumer.
func ParseTopic(values map[string]string) (Topic, error) {
	var spec Topic
	var err error
	counts := []struct {
		attribute string
		target    *uint64
	}{
		{AttributeMinActivePartitions, &spec.MinActivePartitions},
		{AttributeMaxActivePartitions, &spec.MaxActivePartitions},
		{AttributeWriteSpeed, &spec.PartitionWriteSpeedBytesPerSecond},
		{AttributeWriteBurst, &spec.PartitionWriteBurstBytes},
	}
	for _, count := range counts {
		if *count.target, err = positive(values, count.attribute); err != nil {
			return Topic{}, err
		}
	}
	if raw, ok := present(values, AttributeStrategy); ok {
		strategy := strings.ToLower(raw)
		if !slices.Contains(strategies(), strategy) {
			return Topic{}, &TopicError{Attribute: AttributeStrategy, Value: raw,
				Reason: "takes one of " + strings.Join(strategies(), ", ")}
		}
		spec.AutoPartitioningStrategy = strategy
	}
	if spec.AutoPartitioningUpUtilizationPercent, err = percent(values, AttributeUpUtilizationPercent); err != nil {
		return Topic{}, err
	}
	if spec.AutoPartitioningDownUtilizationPercent, err = percent(values, AttributeDownUtilizationPercent); err != nil {
		return Topic{}, err
	}
	if spec.AutoPartitioningStabilizationWindow, err = topicInterval(values, AttributeStabilizationWindow); err != nil {
		return Topic{}, err
	}
	if spec.RetentionPeriod, err = topicInterval(values, AttributeRetentionPeriod); err != nil {
		return Topic{}, err
	}
	if raw, ok := present(values, AttributeSupportedCodecs); ok {
		if spec.SupportedCodecs, err = parseCodecs(raw); err != nil {
			return Topic{}, err
		}
	}
	if reason := topicSettingsRefusal(spec); reason != nil {
		return Topic{}, reason
	}
	return spec, nil
}

func topicSettingsRefusal(spec Topic) *TopicError {
	if !autoPartitioned(spec.AutoPartitioningStrategy) {
		shaping := []struct {
			attribute string
			set       bool
		}{
			{AttributeMaxActivePartitions, spec.MaxActivePartitions != 0},
			{AttributeUpUtilizationPercent, spec.AutoPartitioningUpUtilizationPercent != 0},
			{AttributeDownUtilizationPercent, spec.AutoPartitioningDownUtilizationPercent != 0},
			{AttributeStabilizationWindow, spec.AutoPartitioningStabilizationWindow != ""},
		}
		for _, setting := range shaping {
			if setting.set {
				return &TopicError{Attribute: setting.attribute,
					Reason: "it shapes auto-partitioning, which this topic does not enable; declare " +
						AttributeStrategy + " as " + StrategyScaleUp + ", " + StrategyScaleUpAndDown + " or " +
						StrategyPaused}
			}
		}
	}
	minimum := max(spec.MinActivePartitions, defaultMinActivePartitions)
	if spec.MaxActivePartitions != 0 && spec.MaxActivePartitions < minimum {
		return &TopicError{Attribute: AttributeMaxActivePartitions,
			Value: strconv.FormatUint(spec.MaxActivePartitions, 10),
			Reason: fmt.Sprintf("it is below min_active_partitions, %d, which YDB refuses "+
				"(`Invalid total partition count specified`)", minimum)}
	}
	return nil
}

func autoPartitioned(strategy string) bool {
	return strategy != "" && strategy != StrategyDisabled
}

// ParseConsumer reads one consumer out of values, keyed by attribute name.
func ParseConsumer(values map[string]string) (Consumer, error) {
	consumer := Consumer{Name: strings.TrimSpace(values[TopicName])}
	if consumer.Name == "" {
		return Consumer{}, &TopicError{Attribute: TopicName, Reason: "a consumer needs a name"}
	}
	if strings.Contains(consumer.Name, "/") {
		return Consumer{}, &TopicError{Attribute: TopicName, Value: consumer.Name,
			Reason: "a consumer's name cannot hold a slash"}
	}
	var err error
	if consumer.Important, err = topicBoolean(values, AttributeImportant); err != nil {
		return Consumer{}, err
	}
	if raw, ok := present(values, AttributeReadFrom); ok {
		if consumer.ReadFrom, err = normalizeReadFrom(raw); err != nil {
			return Consumer{}, &TopicError{Attribute: AttributeReadFrom, Value: raw, Reason: err.Error()}
		}
	}
	if raw, ok := present(values, AttributeSupportedCodecs); ok {
		if consumer.SupportedCodecs, err = parseCodecs(raw); err != nil {
			return Consumer{}, err
		}
	}
	if consumer.AvailabilityPeriod, err = topicInterval(values, AttributeAvailabilityPeriod); err != nil {
		return Consumer{}, err
	}
	if consumer.Important && consumer.AvailabilityPeriod != "" {
		return Consumer{}, &TopicError{Attribute: AttributeAvailabilityPeriod,
			Value: consumer.AvailabilityPeriod, Reason: "YDB keeps every unread record for an important consumer, " +
				"so it takes no availability period as well (`has both an important flag and a limited " +
				"availability_period, which are mutually exclusive`)"}
	}
	return consumer, nil
}

// present returns the trimmed value of attribute and whether one was given.
func present(values map[string]string, attribute string) (string, bool) {
	raw, ok := values[attribute]
	if !ok {
		return "", false
	}
	return strings.TrimSpace(raw), true
}

func topicBoolean(values map[string]string, attribute string) (bool, error) {
	raw, ok := present(values, attribute)
	if !ok {
		return false, nil
	}
	switch strings.ToLower(raw) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, &TopicError{Attribute: attribute, Value: raw, Reason: "takes true or false"}
	}
}

func positive(values map[string]string, attribute string) (uint64, error) {
	raw, ok := present(values, attribute)
	if !ok {
		return 0, nil
	}
	count, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || count == 0 || count > 1<<62 {
		return 0, &TopicError{Attribute: attribute, Value: raw, Reason: "takes a whole number above zero"}
	}
	return count, nil
}

func percent(values map[string]string, attribute string) (uint32, error) {
	raw, ok := present(values, attribute)
	if !ok {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 10, 32)
	if err != nil || value == 0 || value > 100 {
		return 0, &TopicError{Attribute: attribute, Value: raw, Reason: "takes a whole percentage from 1 to 100"}
	}
	return uint32(value), nil
}

func topicInterval(values map[string]string, attribute string) (string, error) {
	raw, ok := present(values, attribute)
	if !ok {
		return "", nil
	}
	if _, err := topicSeconds(raw); err != nil {
		return "", &TopicError{Attribute: attribute, Value: raw, Reason: err.Error()}
	}
	return raw, nil
}

var codecNumbers = map[string]bool{"raw": true, "gzip": true, "lzop": true, "zstd": true, "custom": true}

func parseCodecs(raw string) ([]string, error) {
	var codecs []string
	for part := range strings.SplitSeq(raw, ",") {
		codec := strings.ToLower(strings.TrimSpace(part))
		if !codecNumbers[codec] {
			return nil, &TopicError{Attribute: AttributeSupportedCodecs, Value: raw,
				Reason: "takes a comma-separated list of " + strings.Join(CodecNames(), ", ") +
					"; YDB keeps a list naming any other codec as no list at all"}
		}
		if slices.Contains(codecs, codec) {
			return nil, &TopicError{Attribute: AttributeSupportedCodecs, Value: raw,
				Reason: "names codec " + codec + " twice"}
		}
		codecs = append(codecs, codec)
	}
	return codecs, nil
}

// TopicSeconds reads one of a topic's intervals as whole seconds, with the
// refusals a declaration's parse gives.
func TopicSeconds(text string) (uint64, error) { return topicSeconds(text) }

// topicSeconds reads one of a topic's intervals as whole seconds.
func topicSeconds(text string) (uint64, error) {
	micros, ok := ydbtype.ParseInterval(text)
	switch {
	case !ok:
		return 0, errors.New("takes an ISO 8601 duration such as PT12H or P1D")
	case micros <= 0:
		return 0, errors.New("is no positive time, and YDB takes only a positive interval")
	case micros%int64(time.Second/time.Microsecond) != 0:
		return 0, errors.New("YDB keeps whole seconds, and drops the fraction of this one")
	}
	return uint64(micros / int64(time.Second/time.Microsecond)), nil
}

func normalizeReadFrom(raw string) (string, error) {
	instant, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("takes an RFC 3339 time such as 2026-01-01T00:00:00Z")
	}
	if instant.Nanosecond() != 0 {
		return "", errors.New("YDB keeps whole seconds, and drops the fraction of this one")
	}
	if instant.Unix() == 0 {
		return "", nil
	}
	return instant.UTC().Format(time.RFC3339), nil
}

// errAbsolutePath is the refusal of a path written from the server root.
var errAbsolutePath = errors.New("the path starts with a slash; write the path relative to the database root, " +
	"without the database's own path")

// splitPath reads a path relative to the database root into its directory
// and its name.
func splitPath(written string) (directory, name string, err error) {
	trimmed := strings.TrimSpace(written)
	if strings.HasPrefix(trimmed, "/") {
		return "", "", errAbsolutePath
	}
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[:index], trimmed[index+1:], nil
	}
	return "", trimmed, nil
}

// pathIdentity reports whether a directory and a name are an object YDB
// keeps at a path relative to the database root: a name without a slash, and
// a clean directory.
func pathIdentity(schema, name string) bool {
	return !(strings.TrimSpace(name) == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || strings.ContainsRune(schema, 0) ||
		!utf8.ValidString(schema) || !utf8.ValidString(name) ||
		(schema != "" && (path.IsAbs(schema) || path.Clean(schema) != schema || schema == "." || schema == ".." || strings.HasPrefix(schema, "../"))))
}

// Path is an object's directory relative to the database root and its name.
type Path struct{ Schema, Name string }

// ParseTopicPath reads the path of a topic relative to the database root.
func ParseTopicPath(written string) (Path, error) {
	schema, name, err := splitPath(written)
	if err != nil {
		return Path{}, fmt.Errorf("%q is not a topic path (dir/name): %w: %w", written, errInvalidValue, err)
	}
	if !pathIdentity(schema, name) {
		return Path{}, fmt.Errorf("%q is not a topic path (dir/name): %w: a topic requires a schema-scoped YDB identity", written, errInvalidValue)
	}
	return Path{Schema: schema, Name: name}, nil
}

// TopicDisplay names a topic by its path, unquoted, for messages.
func TopicDisplay(schema, name string) string {
	if schema == "" {
		return name
	}
	return strings.TrimRight(schema, "/") + "/" + name
}

// CheckTopic refuses a topic YDB would not keep as written: its name and
// directory, its settings, and each of its consumers once.
func CheckTopic(schema, name string, spec Topic) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return &TopicError{Attribute: TopicName, Reason: "a topic needs a name"}
	case strings.Contains(name, "/"):
		return &TopicError{Attribute: TopicName, Value: name, Reason: "holds a slash; name the directory with " + TopicSchema}
	case !pathIdentity("", name):
		return &TopicError{Attribute: TopicName, Value: name, Reason: "is not a path segment"}
	}
	schema = strings.TrimSpace(schema)
	switch {
	case strings.HasPrefix(schema, "/"):
		return &TopicError{Attribute: TopicSchema, Value: schema,
			Reason: "starts with a slash; name the directory relative to the database root, without the database's own path"}
	case schema != "" && !pathIdentity(schema, "topic"):
		return &TopicError{Attribute: TopicSchema, Value: schema,
			Reason: "is not a directory path relative to the database root; write it without a trailing slash or an empty, . or .. segment"}
	}
	return ValidateTopic(spec)
}

// ValidateTopic holds a topic's settings and consumers to the rules a
// declaration is read with.
func ValidateTopic(spec Topic) error {
	if _, err := ParseTopic(topicValues(spec)); err != nil {
		return err
	}
	names := make(map[string]bool, len(spec.Consumers))
	for _, consumer := range spec.Consumers {
		if names[consumer.Name] {
			return fmt.Errorf("two of its consumers are named %q, and YDB names a consumer once per topic "+
				"(`Consumer %s defined more than once`)", consumer.Name, consumer.Name)
		}
		names[consumer.Name] = true
		if _, err := ParseConsumer(ConsumerValues(consumer)); err != nil {
			return fmt.Errorf("consumer %q: %w", consumer.Name, err)
		}
	}
	return nil
}

func topicValues(spec Topic) map[string]string {
	values := make(map[string]string)
	counts := []struct {
		attribute string
		value     uint64
	}{
		{AttributeMinActivePartitions, spec.MinActivePartitions},
		{AttributeMaxActivePartitions, spec.MaxActivePartitions},
		{AttributeWriteSpeed, spec.PartitionWriteSpeedBytesPerSecond},
		{AttributeWriteBurst, spec.PartitionWriteBurstBytes},
		{AttributeUpUtilizationPercent, uint64(spec.AutoPartitioningUpUtilizationPercent)},
		{AttributeDownUtilizationPercent, uint64(spec.AutoPartitioningDownUtilizationPercent)},
	}
	for _, count := range counts {
		if count.value != 0 {
			values[count.attribute] = strconv.FormatUint(count.value, 10)
		}
	}
	for _, text := range []struct{ attribute, value string }{
		{AttributeStrategy, spec.AutoPartitioningStrategy},
		{AttributeStabilizationWindow, spec.AutoPartitioningStabilizationWindow},
		{AttributeRetentionPeriod, spec.RetentionPeriod},
	} {
		if text.value != "" {
			values[text.attribute] = text.value
		}
	}
	if len(spec.SupportedCodecs) > 0 {
		values[AttributeSupportedCodecs] = strings.Join(spec.SupportedCodecs, ",")
	}
	return values
}

// ConsumerValues writes a consumer back as the attribute values
// [ParseConsumer] reads.
func ConsumerValues(consumer Consumer) map[string]string {
	values := map[string]string{TopicName: consumer.Name}
	if consumer.Important {
		values[AttributeImportant] = "true"
	}
	if consumer.ReadFrom != "" {
		values[AttributeReadFrom] = consumer.ReadFrom
	}
	if len(consumer.SupportedCodecs) > 0 {
		values[AttributeSupportedCodecs] = strings.Join(consumer.SupportedCodecs, ",")
	}
	if consumer.AvailabilityPeriod != "" {
		values[AttributeAvailabilityPeriod] = consumer.AvailabilityPeriod
	}
	return values
}
