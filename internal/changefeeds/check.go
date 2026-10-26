package changefeeds

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// The capability keys a changefeed's statements need, as the host names them.
const (
	// CapabilityChangefeeds is a target that keeps changefeeds and the
	// consumers of their topics.
	CapabilityChangefeeds = "changefeeds"
	// CapabilityUserSIDs is a target that takes USER_SIDS = TRUE (YDB 26.1
	// and later; `Unknown changefeed setting: USER_SIDS` before).
	CapabilityUserSIDs = "changefeed_user_sids"
	// CapabilitySchemaChanges is a target that takes SCHEMA_CHANGES = TRUE
	// (YDB 25.3 and later; `Unknown changefeed setting: SCHEMA_CHANGES`
	// before).
	CapabilitySchemaChanges = "changefeed_schema_changes"
	// CapabilityTopicAutoPartitioning is a target that takes
	// TOPIC_AUTO_PARTITIONING = 'ENABLED' (YDB 25.2 and later, or 25.1 with
	// EnableTopicAutopartitioningForCDC; `Topic autopartitioning for CDC is
	// disabled` otherwise).
	CapabilityTopicAutoPartitioning = "changefeed_topic_auto_partitioning"
)

// Refusal says why a changefeed cannot be written on a target: Key is the
// capability it needs and the target lacks, or empty when YDB refuses it
// whatever the target, and Reason then says why.
type Refusal struct {
	// Subject names what is refused.
	Subject string
	// Key is the capability the declaration needs, empty for a refusal YDB
	// makes on every line.
	Key string
	// Reason is why YDB refuses it, for a refusal without a key.
	Reason string
}

// Message writes the refusal in the host's words. A Refusal is not an error,
// so a nil one never reads as a failure.
func (r *Refusal) Message() string {
	if r.Key != "" {
		return fmt.Sprintf("%s, which requires target capability %s, unavailable on this ydb target", r.Subject, r.Key)
	}
	return fmt.Sprintf("%s: %s", r.Subject, r.Reason)
}

// Subject names a changefeed of a table for a refusal.
func Subject(table, name string) string { return fmt.Sprintf("changefeed %q of table %q", name, table) }

// Check reports why spec cannot be written on a target holding capabilities,
// or nil when it can. table names the table in the subject. An absent
// capability is one the target was not shown to hold.
//
// It holds what a declaration's parse cannot know: the options a line lacks,
// each through its capability key, and the shapes YDB refuses on every line,
// measured on 25.1.4.7 and 26.2.1.14 -- DEBEZIUM_JSON in UPDATES mode
// (`DEBEZIUM_JSON format incompatible with specified stream mode`), and with
// virtual or resolved timestamps or schema change records
// (`VIRTUAL_TIMESTAMPS incompatible with specified stream format`, and the
// same for RESOLVED_TIMESTAMPS and, on 26.2.1.14, SCHEMA_CHANGES), and a
// disabled changefeed, since no statement disables one. A spec built by hand
// is held to the parse's rules too, because nothing else checks it.
func Check(table string, spec Feed, capabilities map[string]bool) *Refusal {
	subject := Subject(table, spec.Name)
	if !capabilities[CapabilityChangefeeds] {
		return &Refusal{Subject: subject, Key: CapabilityChangefeeds}
	}
	if reason := shapeRefusal(spec); reason != "" {
		return &Refusal{Subject: subject, Reason: reason}
	}
	for _, option := range []struct {
		set  bool
		key  string
		what string
	}{
		{spec.UserSIDs, CapabilityUserSIDs, "USER_SIDS"},
		{spec.SchemaChanges, CapabilitySchemaChanges, "SCHEMA_CHANGES"},
		{spec.TopicAutoPartitioning, CapabilityTopicAutoPartitioning, "TOPIC_AUTO_PARTITIONING"},
	} {
		if option.set && !capabilities[option.key] {
			return &Refusal{Subject: subject + " takes " + option.what, Key: option.key}
		}
	}
	names := make(map[string]bool, len(spec.Consumers))
	for _, consumer := range spec.Consumers {
		consumerSubject := fmt.Sprintf("consumer %q of %s", consumer.Name, subject)
		if names[consumer.Name] {
			return &Refusal{Subject: subject, Reason: fmt.Sprintf("two of its consumers are named %q, and YDB "+
				"names a consumer once per topic (`Duplicate consumer name`)", consumer.Name)}
		}
		names[consumer.Name] = true
		if reason := consumerRefusal(consumer); reason != "" {
			return &Refusal{Subject: consumerSubject, Reason: reason}
		}
		if consumer.AvailabilityPeriod != "" && !capabilities[topics.CapabilityAvailabilityPeriod] {
			return &Refusal{Subject: consumerSubject + " takes availability_period", Key: topics.CapabilityAvailabilityPeriod}
		}
	}
	return nil
}

// shapeRefusal says why YDB refuses spec on every line, or is empty.
func shapeRefusal(spec Feed) string {
	switch {
	case strings.TrimSpace(spec.Name) == "" || strings.Contains(spec.Name, "/"):
		return "a changefeed needs a name without a slash (`symbol '/' is not allowed in the path part`)"
	case !slices.Contains(Modes(), strings.ToUpper(spec.Mode)):
		return fmt.Sprintf("its mode %q is none of %s", spec.Mode, strings.Join(Modes(), ", "))
	case !slices.Contains(Formats(), strings.ToUpper(spec.Format)):
		return fmt.Sprintf("its format %q is none of %s", spec.Format, strings.Join(Formats(), ", "))
	case strings.EqualFold(spec.Format, "DEBEZIUM_JSON") && strings.EqualFold(spec.Mode, "UPDATES"):
		return "YDB writes DEBEZIUM_JSON in every mode but UPDATES (`DEBEZIUM_JSON format incompatible with " +
			"specified stream mode`)"
	case strings.EqualFold(spec.Format, "DEBEZIUM_JSON") &&
		(spec.VirtualTimestamps || spec.ResolvedTimestamps != "" || spec.SchemaChanges):
		return "YDB writes DEBEZIUM_JSON with no virtual timestamps, resolved timestamps or schema change records " +
			"(`VIRTUAL_TIMESTAMPS incompatible with specified stream format`, and likewise for the other two)"
	case spec.Disabled:
		return "YDB has no statement that disables a changefeed (`ALTER CHANGEFEED ... DISABLE` answers " +
			"`Name not found: quote` on every measured line)"
	}
	for _, period := range []struct{ attribute, value string }{
		{ydbdecl.AttributeResolvedTimestamps, spec.ResolvedTimestamps},
		{ydbdecl.AttributeFeedRetentionPeriod, spec.RetentionPeriod},
	} {
		if period.value == "" {
			continue
		}
		if _, err := ydbdecl.FeedSeconds(period.value); err != nil {
			return fmt.Sprintf("its %s %q: %v", period.attribute, period.value, err)
		}
	}
	return ""
}

// consumerRefusal says why YDB refuses consumer on every line, or is empty.
func consumerRefusal(consumer ydbdecl.Consumer) string {
	values := map[string]string{ydbdecl.TopicName: consumer.Name}
	if consumer.Important {
		values[ydbdecl.AttributeImportant] = "true"
	}
	if consumer.ReadFrom != "" {
		values[ydbdecl.AttributeReadFrom] = consumer.ReadFrom
	}
	if len(consumer.SupportedCodecs) > 0 {
		values[ydbdecl.AttributeSupportedCodecs] = strings.Join(consumer.SupportedCodecs, ",")
	}
	if consumer.AvailabilityPeriod != "" {
		values[ydbdecl.AttributeAvailabilityPeriod] = consumer.AvailabilityPeriod
	}
	if _, err := ydbdecl.ParseConsumer(values); err != nil {
		return err.Error()
	}
	return ""
}

// NameRefusal says why the changefeeds and the indexes of one table cannot all
// be created, or is empty. A table's changefeeds and indexes are named in one
// namespace, the table's path: measured on 25.1.4.7 and 26.2.1.14, a
// changefeed named after an index answers `unexpected path type ...
// EPathTypeTableIndex`, and an index named after a changefeed the other way
// round.
func NameRefusal(changefeeds []Feed, indexes []string) string {
	seen := make(map[string]bool, len(changefeeds))
	for _, changefeed := range changefeeds {
		if seen[changefeed.Name] {
			return fmt.Sprintf("two of its changefeeds are named %q", changefeed.Name)
		}
		seen[changefeed.Name] = true
		if slices.Contains(indexes, changefeed.Name) {
			return fmt.Sprintf("changefeed %q has the name of one of its indexes, and YDB keeps both under the "+
				"table's path", changefeed.Name)
		}
	}
	return ""
}

// KeyRefusal says why YDB refuses spec on a table whose first key column has
// type firstKeyType, or is empty. A topic declared with more than one
// partition splits the stream by the first key column, which YDB takes only as
// Uint32 or Uint64: measured on 25.1.4.7 and 26.2.1.14,
// TOPIC_MIN_ACTIVE_PARTITIONS = 2 on a Utf8 key answers `Unsupported first key
// column type Utf8, only Uint32 and Uint64 are supported`.
func KeyRefusal(spec Feed, firstKeyType string) string {
	if spec.TopicMinActivePartitions <= 1 || firstKeyType == "" {
		return ""
	}
	if firstKeyType == "Uint32" || firstKeyType == "Uint64" {
		return ""
	}
	return fmt.Sprintf("its topic starts with %d partitions, which YDB splits by the first key column, and takes "+
		"that column only as Uint32 or Uint64, not %s", spec.TopicMinActivePartitions, firstKeyType)
}
