package changefeeds

import (
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// Equal reports whether two descriptions of a changefeed describe the one YDB
// keeps: the same options, the same retention and the same consumers.
//
// It compares as YDB keeps a changefeed rather than as it was written. A mode,
// a format and a codec compare in any case; an interval compares by the
// seconds it denotes, an empty retention as YDB's 24 hours and an empty
// interval otherwise as none; a read_from compares as an instant, an empty one
// as the start of the epoch YDB reports for it; a consumer's codecs compare as
// a set; and a topic's starting partition count compares only where both
// sides name one, since a changefeed that declares none starts with as many
// partitions as the table has, which no declaration can know.
func Equal(desired, current Feed) bool {
	return !Recreated(desired, current) && !TopicChanged(desired, current)
}

// Recreated reports whether moving a changefeed from current to desired drops
// it and adds it again: an option of `ADD CHANGEFEED` differs, which YDB
// changes in no other way, or current is disabled, which no statement
// reverses.
func Recreated(desired, current Feed) bool {
	switch {
	case !strings.EqualFold(desired.Mode, current.Mode),
		!strings.EqualFold(desired.Format, current.Format),
		desired.VirtualTimestamps != current.VirtualTimestamps,
		intervalSeconds(desired.ResolvedTimestamps, 0) != intervalSeconds(current.ResolvedTimestamps, 0),
		desired.InitialScan != current.InitialScan,
		desired.UserSIDs != current.UserSIDs,
		desired.SchemaChanges != current.SchemaChanges,
		desired.TopicAutoPartitioning != current.TopicAutoPartitioning,
		desired.Disabled != current.Disabled:
		return true
	}
	return desired.TopicMinActivePartitions != 0 && current.TopicMinActivePartitions != 0 &&
		desired.TopicMinActivePartitions != current.TopicMinActivePartitions
}

// TopicChanged reports whether the changefeed's topic holds another retention
// or other consumers than desired says, which `ALTER TOPIC` changes in place.
func TopicChanged(desired, current Feed) bool {
	if RetentionChanged(desired, current) {
		return true
	}
	if len(desired.Consumers) != len(current.Consumers) {
		return true
	}
	for _, want := range desired.Consumers {
		have, found := consumerNamed(current.Consumers, want.Name)
		if !found || !topics.ConsumerEqual(want, have) {
			return true
		}
	}
	return false
}

// ConsumerPositionsLost names the consumers of current whose positions moving
// the topic to desired discards: a consumer desired leaves out is dropped, and
// one whose codec list is to be emptied is dropped and added again. These are
// the decisions [TopicStatements] makes.
func ConsumerPositionsLost(desired, current Feed) []string {
	var names []string
	for _, have := range current.Consumers {
		want, found := consumerNamed(desired.Consumers, have.Name)
		if !found || consumerRecreated(want, have) {
			names = append(names, have.Name)
		}
	}
	slices.Sort(names)
	return names
}

// RetentionChanged reports whether the topic's retention differs, YDB's 24
// hours standing in for a retention either side leaves out.
func RetentionChanged(desired, current Feed) bool {
	return retentionSeconds(desired.Changefeed) != retentionSeconds(current.Changefeed)
}

// ConsumersChanged reports whether a consumer both sides name differs.
func ConsumersChanged(desired, current Feed) bool {
	return slices.ContainsFunc(desired.Consumers, func(want ydbdecl.Consumer) bool {
		have, found := consumerNamed(current.Consumers, want.Name)
		return found && !topics.ConsumerEqual(want, have)
	})
}

// consumerRecreated is shared by the statements and the positions a change
// loses: neither may report a consumer's position kept while the other drops
// it. YDB keeps a consumer's codecs once it has any, so emptying them drops the
// consumer and adds it again.
func consumerRecreated(desired, current ydbdecl.Consumer) bool {
	return len(desired.SupportedCodecs) == 0 && len(current.SupportedCodecs) > 0
}

// retentionSeconds is a changefeed's retention in seconds, YDB's default where
// it names none.
func retentionSeconds(spec ydbdecl.Changefeed) uint64 {
	return intervalSeconds(spec.RetentionPeriod, DefaultRetentionSeconds)
}

// intervalSeconds reads an interval, giving empty the value fallback. A value
// this package cannot read compares as zero: it cannot equal a readable one,
// and a declaration holding one is refused before it is compared.
func intervalSeconds(text string, fallback uint64) uint64 {
	if strings.TrimSpace(text) == "" {
		return fallback
	}
	value, err := ydbdecl.FeedSeconds(text)
	if err != nil {
		return 0
	}
	return value
}

// consumerNamed finds a consumer by name.
func consumerNamed(consumers []ydbdecl.Consumer, name string) (ydbdecl.Consumer, bool) {
	index := slices.IndexFunc(consumers, func(consumer ydbdecl.Consumer) bool { return consumer.Name == name })
	if index < 0 {
		return ydbdecl.Consumer{}, false
	}
	return consumers[index], true
}
