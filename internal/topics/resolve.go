package topics

import (
	"slices"
	"strings"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// The values YDB gives a new topic for a setting its declaration leaves out,
// measured on 25.1.4.7 and 26.2.1.14.
const (
	defaultMinActivePartitions        = 1
	defaultRetentionSeconds           = 86400
	defaultWriteSpeed                 = 1 << 20
	defaultUpUtilizationPercent       = 90
	defaultDownUtilizationPercent     = 30
	defaultStabilizationWindowSeconds = 300
)

// Settings is a topic's settings with every one resolved: what the server
// holds, rather than what a declaration names. A declaration and a
// description are each resolved into Settings and compared as Settings, so a
// declaration naming a setting at the value a new topic takes and one leaving
// it out are the same topic.
type Settings struct {
	MinActivePartitions uint64
	Strategy            string
	// MaxActivePartitions, the thresholds and the window are zero while
	// Strategy is disabled, since YDB keeps none then.
	MaxActivePartitions        uint64
	UpUtilizationPercent       uint32
	DownUtilizationPercent     uint32
	StabilizationWindowSeconds uint64
	RetentionSeconds           uint64
	WriteSpeed                 uint64
	WriteBurst                 uint64
	// Codecs is a set: lower case, sorted, without repeats. Nil takes any.
	Codecs []string
}

// AutoPartitioned reports a topic whose auto-partitioning is enabled or
// paused, which is when the maximum and the thresholds mean something.
func (s Settings) AutoPartitioned() bool {
	return s.Strategy != "" && s.Strategy != ydbdecl.StrategyDisabled
}

// Resolve reads spec's settings as the ones a topic takes from it: each
// setting spec names, and the value YDB gives a new topic for each it leaves
// out. The write burst a declaration leaves out is the write speed, because
// YDB gives a new topic a burst of its speed. The thresholds a declaration
// leaves out are 90%, 30% and five minutes, which YDB gives a topic created
// with a strategy; a topic that was given its strategy later and kept 80% and
// 20% differs from that declaration, so the change names the thresholds and
// the topic converges.
func Resolve(spec ydbdecl.Topic) Settings {
	settings := Settings{
		MinActivePartitions: max(spec.MinActivePartitions, defaultMinActivePartitions),
		Strategy:            strings.ToLower(spec.AutoPartitioningStrategy),
		RetentionSeconds:    intervalSeconds(spec.RetentionPeriod, defaultRetentionSeconds),
		WriteSpeed:          spec.PartitionWriteSpeedBytesPerSecond,
		WriteBurst:          spec.PartitionWriteBurstBytes,
		Codecs:              codecSet(spec.SupportedCodecs),
	}
	if settings.Strategy == "" {
		settings.Strategy = ydbdecl.StrategyDisabled
	}
	if settings.WriteSpeed == 0 {
		settings.WriteSpeed = defaultWriteSpeed
	}
	if settings.WriteBurst == 0 {
		settings.WriteBurst = settings.WriteSpeed
	}
	if !settings.AutoPartitioned() {
		return settings
	}
	settings.MaxActivePartitions = max(spec.MaxActivePartitions, settings.MinActivePartitions)
	settings.UpUtilizationPercent = valueOr(spec.AutoPartitioningUpUtilizationPercent, defaultUpUtilizationPercent)
	settings.DownUtilizationPercent = valueOr(spec.AutoPartitioningDownUtilizationPercent, defaultDownUtilizationPercent)
	settings.StabilizationWindowSeconds = intervalSeconds(spec.AutoPartitioningStabilizationWindow, defaultStabilizationWindowSeconds)
	return settings
}

// Equal reports whether two descriptions of a topic describe the one YDB
// holds: the same resolved settings and the same consumers by name, each the
// same as YDB keeps it.
func Equal(desired, current ydbdecl.Topic) bool {
	return SettingsEqual(desired, current) && len(Compare(desired, current).Consumers()) == 0
}

// SettingsEqual reports whether two descriptions of a topic resolve to the
// same settings, consumers aside.
func SettingsEqual(desired, current ydbdecl.Topic) bool {
	a, b := Resolve(desired), Resolve(current)
	return a.MinActivePartitions == b.MinActivePartitions && a.Strategy == b.Strategy && a.MaxActivePartitions == b.MaxActivePartitions &&
		a.UpUtilizationPercent == b.UpUtilizationPercent && a.DownUtilizationPercent == b.DownUtilizationPercent &&
		a.StabilizationWindowSeconds == b.StabilizationWindowSeconds && a.RetentionSeconds == b.RetentionSeconds &&
		a.WriteSpeed == b.WriteSpeed && a.WriteBurst == b.WriteBurst && slices.Equal(a.Codecs, b.Codecs)
}

// ConsumerChanges is how the consumers of two descriptions of one topic
// differ, each list in the order the desired side, or for a removal the
// current side, lists them.
type ConsumerChanges struct {
	// Added are the consumers only the desired side has.
	Added []string
	// Removed are the consumers only the current side has.
	Removed []string
	// Changed are the consumers both sides have with settings YDB changes in
	// place.
	Changed []string
	// Restarted are the consumers both sides have whose change YDB cannot
	// make in place: the desired side takes any codec where the current one
	// lists some, and no statement empties a consumer's codec list. The
	// consumer is dropped and added again, and its position in the topic is
	// lost.
	Restarted []string
}

// Consumers lists every consumer the changes touch.
func (c ConsumerChanges) Consumers() []string {
	return slices.Concat(c.Added, c.Removed, c.Changed, c.Restarted)
}

// Compare reads how the consumers of desired differ from those of current.
func Compare(desired, current ydbdecl.Topic) ConsumerChanges {
	var changes ConsumerChanges
	for _, have := range current.Consumers {
		if _, found := consumerNamed(desired.Consumers, have.Name); !found {
			changes.Removed = append(changes.Removed, have.Name)
		}
	}
	for _, want := range desired.Consumers {
		have, found := consumerNamed(current.Consumers, want.Name)
		switch {
		case !found:
			changes.Added = append(changes.Added, want.Name)
		case ConsumerEqual(want, have):
		case len(want.SupportedCodecs) == 0 && len(have.SupportedCodecs) > 0:
			changes.Restarted = append(changes.Restarted, want.Name)
		default:
			changes.Changed = append(changes.Changed, want.Name)
		}
	}
	return changes
}

// ConsumerEqual compares two consumers of one name as YDB keeps them: a
// read_from left out is the start of the epoch, codecs compare as a set, and
// an availability period compares by the seconds it denotes.
func ConsumerEqual(a, b ydbdecl.Consumer) bool {
	return a.Important == b.Important && readFromInstant(a.ReadFrom).Equal(readFromInstant(b.ReadFrom)) &&
		slices.Equal(codecSet(a.SupportedCodecs), codecSet(b.SupportedCodecs)) &&
		intervalSeconds(a.AvailabilityPeriod, 0) == intervalSeconds(b.AvailabilityPeriod, 0)
}

func consumerNamed(consumers []ydbdecl.Consumer, name string) (ydbdecl.Consumer, bool) {
	index := slices.IndexFunc(consumers, func(consumer ydbdecl.Consumer) bool { return consumer.Name == name })
	if index < 0 {
		return ydbdecl.Consumer{}, false
	}
	return consumers[index], true
}

// readFromInstant reads a read_from, the start of the epoch where it is empty
// and the zero time where it does not parse.
func readFromInstant(text string) time.Time {
	if strings.TrimSpace(text) == "" {
		return time.Unix(0, 0)
	}
	instant, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
	if err != nil {
		return time.Time{}
	}
	return instant
}

// codecSet is a codec list folded to lower case, sorted and without repeats.
func codecSet(codecs []string) []string {
	if len(codecs) == 0 {
		return nil
	}
	set := make([]string, 0, len(codecs))
	for _, codec := range codecs {
		set = append(set, strings.ToLower(strings.TrimSpace(codec)))
	}
	slices.Sort(set)
	return slices.Compact(set)
}

// intervalSeconds reads an interval as whole seconds, or fallback for an
// empty one or one the declaration's parse refuses.
func intervalSeconds(text string, fallback uint64) uint64 {
	if strings.TrimSpace(text) == "" {
		return fallback
	}
	seconds, err := ydbdecl.TopicSeconds(text)
	if err != nil {
		return fallback
	}
	return seconds
}

func valueOr(value, fallback uint32) uint32 {
	if value == 0 {
		return fallback
	}
	return value
}
