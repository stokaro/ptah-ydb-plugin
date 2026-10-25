// Package topics reads a standalone YDB topic as the settings and consumers
// Ptah's embedded YDB engine keeps for one, and writes the statements that
// create, change and drop it.
//
// Both representations of the provider's topic model, the native description
// inspection reads and the desired intent a declaration decodes into, are read
// into the host's topic specification (ydbdecl.Topic), and the comparison, the
// refusals and the statements work on that specification, ported from the
// host's dialect/ydb/ydbtopic and internal/dbschema/ydb with its rules and
// words. A topic is therefore compared, refused and written as the embedded
// engine compares, refuses and writes it.
package topics

import (
	"errors"
	"fmt"
	"strings"
	"time"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// codecs maps the codec names a declaration takes to YDB's codec numbers.
var codecs = map[string]int32{"raw": int32(Ydb_Topic.Codec_CODEC_RAW), "gzip": int32(Ydb_Topic.Codec_CODEC_GZIP),
	"lzop": int32(Ydb_Topic.Codec_CODEC_LZOP), "zstd": int32(Ydb_Topic.Codec_CODEC_ZSTD), "custom": int32(Ydb_Topic.Codec_CODEC_CUSTOM)}

// codecName names a codec number, or reports one a declaration cannot name.
func codecName(number int32) (string, bool) {
	for name, value := range codecs {
		if value == number {
			return name, true
		}
	}
	return "", false
}

// strategies maps the strategies a declaration names to YDB's.
var strategies = map[string]Ydb_Topic.AutoPartitioningStrategy{
	ydbdecl.StrategyDisabled:       Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_DISABLED,
	ydbdecl.StrategyScaleUp:        Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_SCALE_UP,
	ydbdecl.StrategyScaleUpAndDown: Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_SCALE_UP_AND_DOWN,
	ydbdecl.StrategyPaused:         Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_PAUSED,
}

// strategyName names a strategy YDB reports, a topic created without one
// being disabled.
func strategyName(strategy Ydb_Topic.AutoPartitioningStrategy) (string, error) {
	if strategy == Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_UNSPECIFIED {
		return ydbdecl.StrategyDisabled, nil
	}
	for name, value := range strategies {
		if value == strategy {
			return name, nil
		}
	}
	return "", fmt.Errorf("its auto-partitioning strategy %d is not one this provider reads", int32(strategy))
}

// FromObserved reads a topic's description as the settings and consumers it
// holds, as the host's reader does: the maximum and the thresholds only where
// auto-partitioning is on, intervals as whole seconds, and no attribute, since
// YDB's own begin with an underscore, change by line and no statement changes
// one. A topic holding a setting the host does not model is refused with the
// host's words, as the host refuses to read it.
func FromObserved(observed *native.DescribeTopicResult) (ydbdecl.Topic, error) {
	if err := refuseUnmodeled(observed); err != nil {
		return ydbdecl.Topic{}, err
	}
	var spec ydbdecl.Topic
	partitioning := observed.GetPartitioningSettings()
	auto := partitioning.GetAutoPartitioningSettings()
	spec.MinActivePartitions = uint64(max(partitioning.GetMinActivePartitions(), 0))
	strategy, err := strategyName(auto.GetStrategy())
	if err != nil {
		return ydbdecl.Topic{}, err
	}
	spec.AutoPartitioningStrategy = strategy
	if strategy != ydbdecl.StrategyDisabled {
		speed := auto.GetPartitionWriteSpeed()
		spec.MaxActivePartitions = uint64(max(partitioning.GetMaxActivePartitions(), 0))
		spec.AutoPartitioningUpUtilizationPercent = uint32(max(speed.GetUpUtilizationPercent(), 0))
		spec.AutoPartitioningDownUtilizationPercent = uint32(max(speed.GetDownUtilizationPercent(), 0))
		if spec.AutoPartitioningStabilizationWindow, err = interval("its stabilization window", speed.GetStabilizationWindow()); err != nil {
			return ydbdecl.Topic{}, err
		}
	}
	if spec.RetentionPeriod, err = interval("its retention period", observed.GetRetentionPeriod()); err != nil {
		return ydbdecl.Topic{}, err
	}
	spec.PartitionWriteSpeedBytesPerSecond = uint64(max(observed.GetPartitionWriteSpeedBytesPerSecond(), 0))
	spec.PartitionWriteBurstBytes = uint64(max(observed.GetPartitionWriteBurstBytes(), 0))
	if spec.SupportedCodecs, err = codecNames("its", observed.GetSupportedCodecs()); err != nil {
		return ydbdecl.Topic{}, err
	}
	for _, consumer := range observed.GetConsumers() {
		decoded, err := observedConsumer(consumer)
		if err != nil {
			return ydbdecl.Topic{}, fmt.Errorf("consumer %q: %w", consumer.GetName(), err)
		}
		spec.Consumers = append(spec.Consumers, decoded)
	}
	return spec, nil
}

// refuseUnmodeled refuses a topic holding a setting the host does not model
// at another value than a topic YQL creates holds. Measured on 25.1.4.7 and
// 26.2.1.14, a topic YQL creates holds none of them, so only a topic another
// tool made, or one in a serverless database, is refused.
func refuseUnmodeled(observed *native.DescribeTopicResult) error {
	for _, setting := range []struct {
		name string
		set  bool
	}{
		{"retention_storage_mb", observed.GetRetentionStorageMb() != 0},
		//nolint:staticcheck // SA1019: the deprecated field is the one being refused.
		{"partition_count_limit", observed.GetPartitioningSettings().GetPartitionCountLimit() != 0},
		{"metering_mode", observed.GetMeteringMode() != Ydb_Topic.MeteringMode_METERING_MODE_UNSPECIFIED},
		{"partition_total_read_speed_bytes_per_second", observed.GetPartitionTotalReadSpeedBytesPerSecond() != 0},
		{"partition_consumer_read_speed_bytes_per_second", observed.GetPartitionConsumerReadSpeedBytesPerSecond() != 0},
		{"metrics_level", observed.MetricsLevel != nil},
	} {
		if setting.set {
			return fmt.Errorf("it holds %s, which Ptah does not model and no YQL statement sets; "+
				"read a schema that does not hold the topic", setting.name)
		}
	}
	return nil
}

// observedConsumer reads one consumer of a description. A consumer's
// attributes are YDB's own (`_service_type`), and a streaming consumer type is
// the one kind of consumer YQL creates; neither is declared.
func observedConsumer(observed *native.Consumer) (ydbdecl.Consumer, error) {
	consumer := ydbdecl.Consumer{Name: observed.GetName(), Important: observed.GetImportant()}
	if readFrom := observed.GetReadFrom(); readFrom != nil {
		if readFrom.GetNanos() != 0 {
			return ydbdecl.Consumer{}, fmt.Errorf("its read_from %s is not a whole second", readFrom.AsTime().Format(time.RFC3339Nano))
		}
		consumer.ReadFrom = formatReadFrom(readFrom.AsTime())
	}
	var err error
	if consumer.SupportedCodecs, err = codecNames("its", observed.GetSupportedCodecs()); err != nil {
		return ydbdecl.Consumer{}, err
	}
	if consumer.AvailabilityPeriod, err = interval("its availability period", observed.GetAvailabilityPeriod()); err != nil {
		return ydbdecl.Consumer{}, err
	}
	return consumer, nil
}

// FromIntent reads a desired topic as the declaration it states. A setting
// the intent leaves at zero is one the declaration leaves out. An intent
// converted from a read states every setting, YDB's attributes among them;
// those are left out, as the reader leaves them out, and a setting no
// declaration can state is refused.
func FromIntent(intent *native.TopicIntent) (ydbdecl.Topic, error) {
	if intent == nil {
		return ydbdecl.Topic{}, errors.New("missing YDB topic declaration")
	}
	for _, setting := range []struct {
		name string
		set  bool
	}{
		{"retention_storage_mb", intent.GetRetentionStorageMb() != 0},
		//nolint:staticcheck // SA1019: the deprecated field is the one being refused.
		{"partition_count_limit", intent.GetPartitioningSettings().GetPartitionCountLimit() != 0},
		{"metering_mode", intent.GetMeteringMode() != Ydb_Topic.MeteringMode_METERING_MODE_UNSPECIFIED},
		{"partition_total_read_speed_bytes_per_second", intent.GetPartitionTotalReadSpeedBytesPerSecond() != 0},
		{"partition_consumer_read_speed_bytes_per_second", intent.GetPartitionConsumerReadSpeedBytesPerSecond() != 0},
		{"metrics_level", intent.MetricsLevel != nil},
	} {
		if setting.set {
			return ydbdecl.Topic{}, fmt.Errorf("the declaration states %s, which no YQL statement sets", setting.name)
		}
	}
	var spec ydbdecl.Topic
	partitioning := intent.GetPartitioningSettings()
	auto := partitioning.GetAutoPartitioningSettings()
	spec.MinActivePartitions = uint64(max(partitioning.GetMinActivePartitions(), 0))
	spec.MaxActivePartitions = uint64(max(partitioning.GetMaxActivePartitions(), 0))
	if auto.GetStrategy() != Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_UNSPECIFIED {
		strategy, err := strategyName(auto.GetStrategy())
		if err != nil {
			return ydbdecl.Topic{}, err
		}
		spec.AutoPartitioningStrategy = strategy
	}
	if spec.AutoPartitioningStrategy == "" || spec.AutoPartitioningStrategy == ydbdecl.StrategyDisabled {
		// YDB reports the maximum and the thresholds of a topic whose
		// auto-partitioning is off, and keeps none of them: an intent read
		// from such a topic states them, and a declaration cannot.
		spec.MaxActivePartitions = 0
	} else {
		speed := auto.GetPartitionWriteSpeed()
		spec.AutoPartitioningUpUtilizationPercent = uint32(max(speed.GetUpUtilizationPercent(), 0))
		spec.AutoPartitioningDownUtilizationPercent = uint32(max(speed.GetDownUtilizationPercent(), 0))
		var err error
		if spec.AutoPartitioningStabilizationWindow, err = interval("its stabilization window", speed.GetStabilizationWindow()); err != nil {
			return ydbdecl.Topic{}, err
		}
	}
	var err error
	if spec.RetentionPeriod, err = interval("its retention period", intent.GetRetentionPeriod()); err != nil {
		return ydbdecl.Topic{}, err
	}
	spec.PartitionWriteSpeedBytesPerSecond = uint64(max(intent.GetPartitionWriteSpeedBytesPerSecond(), 0))
	spec.PartitionWriteBurstBytes = uint64(max(intent.GetPartitionWriteBurstBytes(), 0))
	if spec.SupportedCodecs, err = codecNames("its", intent.GetSupportedCodecs()); err != nil {
		return ydbdecl.Topic{}, err
	}
	for _, consumer := range intent.GetConsumers() {
		decoded := ydbdecl.Consumer{Name: consumer.GetName(), Important: consumer.GetImportant()}
		if readFrom := consumer.GetReadFrom(); readFrom != nil {
			if readFrom.GetNanos() != 0 {
				return ydbdecl.Topic{}, fmt.Errorf("consumer %q: its read_from %s is not a whole second", consumer.GetName(), readFrom.AsTime().Format(time.RFC3339Nano))
			}
			decoded.ReadFrom = formatReadFrom(readFrom.AsTime())
		}
		if decoded.SupportedCodecs, err = codecNames("its", consumer.GetSupportedCodecs()); err != nil {
			return ydbdecl.Topic{}, fmt.Errorf("consumer %q: %w", consumer.GetName(), err)
		}
		if decoded.AvailabilityPeriod, err = interval("its availability period", consumer.GetAvailabilityPeriod()); err != nil {
			return ydbdecl.Topic{}, fmt.Errorf("consumer %q: %w", consumer.GetName(), err)
		}
		spec.Consumers = append(spec.Consumers, decoded)
	}
	if err := ydbdecl.ValidateTopic(spec); err != nil {
		return ydbdecl.Topic{}, err
	}
	return spec, nil
}

// Intent writes a declared topic as the provider's desired model: each
// setting the declaration names, and zero for each it leaves out. Consumers
// keep their declared order.
func Intent(spec ydbdecl.Topic) (*native.TopicIntent, error) {
	if err := ydbdecl.ValidateTopic(spec); err != nil {
		return nil, err
	}
	intent := &native.TopicIntent{PartitioningSettings: &Ydb_Topic.PartitioningSettings{
		MinActivePartitions: int64(spec.MinActivePartitions), MaxActivePartitions: int64(spec.MaxActivePartitions)}}
	if spec.AutoPartitioningStrategy != "" {
		auto := &Ydb_Topic.AutoPartitioningSettings{Strategy: strategies[strings.ToLower(spec.AutoPartitioningStrategy)]}
		if spec.AutoPartitioningUpUtilizationPercent != 0 || spec.AutoPartitioningDownUtilizationPercent != 0 || spec.AutoPartitioningStabilizationWindow != "" {
			auto.PartitionWriteSpeed = &Ydb_Topic.AutoPartitioningWriteSpeedStrategy{
				UpUtilizationPercent: int32(spec.AutoPartitioningUpUtilizationPercent), DownUtilizationPercent: int32(spec.AutoPartitioningDownUtilizationPercent)}
			if spec.AutoPartitioningStabilizationWindow != "" {
				auto.PartitionWriteSpeed.StabilizationWindow = durationOf(spec.AutoPartitioningStabilizationWindow)
			}
		}
		intent.PartitioningSettings.AutoPartitioningSettings = auto
	}
	if spec.RetentionPeriod != "" {
		intent.RetentionPeriod = durationOf(spec.RetentionPeriod)
	}
	intent.PartitionWriteSpeedBytesPerSecond = int64(spec.PartitionWriteSpeedBytesPerSecond)
	intent.PartitionWriteBurstBytes = int64(spec.PartitionWriteBurstBytes)
	intent.SupportedCodecs = codecList(spec.SupportedCodecs)
	for _, consumer := range spec.Consumers {
		declared := &native.ConsumerIntent{Name: consumer.Name, Important: consumer.Important, SupportedCodecs: codecList(consumer.SupportedCodecs)}
		if consumer.ReadFrom != "" {
			instant, err := time.Parse(time.RFC3339, consumer.ReadFrom)
			if err != nil {
				return nil, fmt.Errorf("consumer %q: invalid read_from %q", consumer.Name, consumer.ReadFrom)
			}
			declared.ReadFrom = timestamppb.New(instant)
		}
		if consumer.AvailabilityPeriod != "" {
			declared.AvailabilityPeriod = durationOf(consumer.AvailabilityPeriod)
		}
		intent.Consumers = append(intent.Consumers, declared)
	}
	return intent, nil
}

// Keep returns the declaration that keeps a read topic as it is, for a topic
// the desired source does not describe: the settings and consumers it holds,
// without YDB's attributes and access metadata, which no statement changes.
func Keep(observed *native.DescribeTopicResult) (*native.TopicIntent, error) {
	spec, err := FromObserved(observed)
	if err != nil {
		return nil, err
	}
	return Intent(spec)
}

// interval writes a duration YDB reports as the ISO 8601 interval a
// declaration takes, or empty for none. A fraction of a second is refused,
// because a declaration keeps whole seconds and would change the setting.
func interval(subject string, duration *durationpb.Duration) (string, error) {
	if duration == nil {
		return "", nil
	}
	if duration.GetNanos() != 0 || duration.GetSeconds() < 0 {
		return "", fmt.Errorf("%s is %s, which is not a whole number of seconds a declaration can hold", subject, duration.AsDuration())
	}
	if duration.GetSeconds() == 0 {
		return "", nil
	}
	return FormatSeconds(uint64(duration.GetSeconds())), nil
}

// durationOf is an interval a declaration's parse accepted, as a duration.
func durationOf(text string) *durationpb.Duration {
	seconds, err := ydbdecl.TopicSeconds(text)
	if err != nil {
		return nil
	}
	return &durationpb.Duration{Seconds: int64(seconds)}
}

// codecNames names the codecs of a codec list, owner's being "its" for the
// topic's own list.
func codecNames(owner string, list *Ydb_Topic.SupportedCodecs) ([]string, error) {
	var names []string
	for _, number := range list.GetCodecs() {
		name, known := codecName(number)
		if !known {
			return nil, fmt.Errorf("%s codec list names codec %d, which Ptah does not declare", owner, number)
		}
		names = append(names, name)
	}
	return names, nil
}

// codecList writes declared codec names as YDB's list, or nil for none.
func codecList(names []string) *Ydb_Topic.SupportedCodecs {
	if len(names) == 0 {
		return nil
	}
	list := &Ydb_Topic.SupportedCodecs{}
	for _, name := range names {
		list.Codecs = append(list.Codecs, codecs[strings.ToLower(strings.TrimSpace(name))])
	}
	return list
}

// FormatSeconds writes a number of seconds as the ISO 8601 duration YDB's
// Interval reads: 86400 is `P1D`, 129600 `P1DT12H`.
func FormatSeconds(seconds uint64) string {
	const microsPerSecond = uint64(time.Second / time.Microsecond)
	seconds = min(seconds, uint64(1<<63-1)/microsPerSecond)
	return ydbtype.IntervalText(int64(seconds * microsPerSecond))
}

// formatReadFrom writes an instant as a consumer's read_from: RFC 3339 in
// UTC, or empty for the start of the Unix epoch, which is what YDB reports
// for a consumer that declared none.
func formatReadFrom(instant time.Time) string {
	if instant.Unix() == 0 {
		return ""
	}
	return instant.UTC().Format(time.RFC3339)
}
