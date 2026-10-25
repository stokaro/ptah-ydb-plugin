package topics

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// Statements writes what moves a topic at path from current to desired: one
// CREATE TOPIC where current is nil, one DROP TOPIC where desired is nil, and
// the ALTER TOPIC statements otherwise. path is written as one quoted YQL
// path, relative to the database root or absolute.
func Statements(path string, current, desired *ydbdecl.Topic) []string {
	switch {
	case current == nil && desired == nil:
		return nil
	case current == nil:
		return []string{CreateStatement(path, *desired)}
	case desired == nil:
		return []string{"DROP TOPIC " + quoteName(path) + ";"}
	default:
		return AlterStatements(path, *desired, *current)
	}
}

// CreateStatement writes the one CREATE TOPIC that creates a topic as spec
// declares it, naming each consumer and each setting spec declares. A setting
// spec leaves out is left to YDB, which gives a new topic the value [Resolve]
// reads the declaration with.
func CreateStatement(path string, spec ydbdecl.Topic) string {
	var b strings.Builder
	b.WriteString("CREATE TOPIC " + quoteName(path))
	if len(spec.Consumers) > 0 {
		consumers := make([]string, len(spec.Consumers))
		for i, consumer := range spec.Consumers {
			consumers[i] = "CONSUMER " + quoteName(consumer.Name) + consumerWith(consumer)
		}
		b.WriteString(" (" + strings.Join(consumers, ", ") + ")")
	}
	if settings := declaredSettings(spec); len(settings) > 0 {
		b.WriteString(" WITH (" + strings.Join(settings, ", ") + ")")
	}
	return b.String() + ";"
}

// AlterStatements writes what moves a topic from previous to desired, and
// nothing when the two are the same topic.
//
// The first statement carries every change YDB makes in one ALTER TOPIC: the
// settings, the consumers dropped, the consumers changed in place and the
// consumers added. A consumer [Compare] reports as restarted is dropped there
// and added again by a second statement, because YDB refuses one ALTER TOPIC
// that names a consumer twice (`consumer 'b' referenced more than once`).
//
// The settings are written whole whenever any of them differs: every setting
// [Resolve] reads, as the value it resolves to. A setting nobody declared
// keeps the value the topic was made with rather than following a change of
// another one, so naming each is what makes one statement converge. No RESET
// is written: YDB 26.2 keeps every value a RESET names and 25.1 refuses one.
func AlterStatements(path string, desired, previous ydbdecl.Topic) []string {
	quoted := quoteName(path)
	changes := Compare(desired, previous)
	var actions []string
	if !SettingsEqual(desired, previous) {
		actions = append(actions, "SET ("+strings.Join(resolvedSettings(Resolve(desired)), ", ")+")")
	}
	for _, consumer := range changes.Removed {
		actions = append(actions, "DROP CONSUMER "+quoteName(consumer))
	}
	for _, consumer := range changes.Restarted {
		actions = append(actions, "DROP CONSUMER "+quoteName(consumer))
	}
	for _, consumer := range changes.Changed {
		want, _ := consumerNamed(desired.Consumers, consumer)
		have, _ := consumerNamed(previous.Consumers, consumer)
		actions = append(actions, "ALTER CONSUMER "+quoteName(consumer)+" SET ("+alteredConsumer(want, have)+")")
	}
	for _, consumer := range changes.Added {
		want, _ := consumerNamed(desired.Consumers, consumer)
		actions = append(actions, "ADD CONSUMER "+quoteName(consumer)+consumerWith(want))
	}
	var statements []string
	if len(actions) > 0 {
		statements = append(statements, "ALTER TOPIC "+quoted+" "+strings.Join(actions, ", ")+";")
	}
	if len(changes.Restarted) > 0 {
		readded := make([]string, len(changes.Restarted))
		for i, consumer := range changes.Restarted {
			want, _ := consumerNamed(desired.Consumers, consumer)
			readded[i] = "ADD CONSUMER " + quoteName(consumer) + consumerWith(want)
		}
		statements = append(statements, "ALTER TOPIC "+quoted+" "+strings.Join(readded, ", ")+";")
	}
	return statements
}

// declaredSettings writes the settings spec names, in the order YDB's
// documentation lists them.
func declaredSettings(spec ydbdecl.Topic) []string {
	var settings []string
	add := func(declared bool, setting string) {
		if declared {
			settings = append(settings, setting)
		}
	}
	add(spec.MinActivePartitions != 0, ydbdecl.AttributeMinActivePartitions+" = "+formatCount(spec.MinActivePartitions))
	add(spec.MaxActivePartitions != 0, ydbdecl.AttributeMaxActivePartitions+" = "+formatCount(spec.MaxActivePartitions))
	add(spec.AutoPartitioningStrategy != "", ydbdecl.AttributeStrategy+" = "+quoteString(spec.AutoPartitioningStrategy))
	add(spec.AutoPartitioningUpUtilizationPercent != 0,
		ydbdecl.AttributeUpUtilizationPercent+" = "+formatCount(uint64(spec.AutoPartitioningUpUtilizationPercent)))
	add(spec.AutoPartitioningDownUtilizationPercent != 0,
		ydbdecl.AttributeDownUtilizationPercent+" = "+formatCount(uint64(spec.AutoPartitioningDownUtilizationPercent)))
	add(spec.AutoPartitioningStabilizationWindow != "",
		ydbdecl.AttributeStabilizationWindow+" = "+intervalLiteral(intervalSeconds(spec.AutoPartitioningStabilizationWindow, 0)))
	add(spec.RetentionPeriod != "", ydbdecl.AttributeRetentionPeriod+" = "+intervalLiteral(intervalSeconds(spec.RetentionPeriod, 0)))
	add(spec.PartitionWriteSpeedBytesPerSecond != 0, ydbdecl.AttributeWriteSpeed+" = "+formatCount(spec.PartitionWriteSpeedBytesPerSecond))
	add(spec.PartitionWriteBurstBytes != 0, ydbdecl.AttributeWriteBurst+" = "+formatCount(spec.PartitionWriteBurstBytes))
	add(len(spec.SupportedCodecs) > 0, ydbdecl.AttributeSupportedCodecs+" = "+codecsLiteral(spec.SupportedCodecs))
	return settings
}

// resolvedSettings writes every setting of settings, the thresholds and the
// maximum only where auto-partitioning is enabled, since YDB keeps no maximum
// otherwise.
func resolvedSettings(settings Settings) []string {
	written := []string{
		ydbdecl.AttributeMinActivePartitions + " = " + formatCount(settings.MinActivePartitions),
		ydbdecl.AttributeStrategy + " = " + quoteString(settings.Strategy),
	}
	if settings.AutoPartitioned() {
		written = append(written,
			ydbdecl.AttributeMaxActivePartitions+" = "+formatCount(settings.MaxActivePartitions),
			ydbdecl.AttributeUpUtilizationPercent+" = "+formatCount(uint64(settings.UpUtilizationPercent)),
			ydbdecl.AttributeDownUtilizationPercent+" = "+formatCount(uint64(settings.DownUtilizationPercent)),
			ydbdecl.AttributeStabilizationWindow+" = "+intervalLiteral(settings.StabilizationWindowSeconds),
		)
	}
	return append(written,
		ydbdecl.AttributeRetentionPeriod+" = "+intervalLiteral(settings.RetentionSeconds),
		ydbdecl.AttributeWriteSpeed+" = "+formatCount(settings.WriteSpeed),
		ydbdecl.AttributeWriteBurst+" = "+formatCount(settings.WriteBurst),
		ydbdecl.AttributeSupportedCodecs+" = "+codecsLiteral(settings.Codecs),
	)
}

// consumerWith writes the WITH clause of a new consumer, naming only what the
// consumer declares, or nothing for a consumer that declares nothing.
func consumerWith(consumer ydbdecl.Consumer) string {
	var settings []string
	if consumer.Important {
		settings = append(settings, ydbdecl.AttributeImportant+" = TRUE")
	}
	if consumer.ReadFrom != "" {
		settings = append(settings, ydbdecl.AttributeReadFrom+" = "+timestampLiteral(consumer.ReadFrom))
	}
	if len(consumer.SupportedCodecs) > 0 {
		settings = append(settings, ydbdecl.AttributeSupportedCodecs+" = "+codecsLiteral(consumer.SupportedCodecs))
	}
	if consumer.AvailabilityPeriod != "" {
		settings = append(settings, ydbdecl.AttributeAvailabilityPeriod+" = "+intervalLiteral(intervalSeconds(consumer.AvailabilityPeriod, 0)))
	}
	if len(settings) == 0 {
		return ""
	}
	return " WITH (" + strings.Join(settings, ", ") + ")"
}

// alteredConsumer writes the settings of one ALTER CONSUMER ... SET. It names
// important and read_from always, the codecs where the consumer is to have
// any, and the availability period where either side has one: zero removes
// it, which 26.2.1.14 takes, and a line without the setting has no consumer
// holding one.
func alteredConsumer(desired, previous ydbdecl.Consumer) string {
	readFrom := desired.ReadFrom
	if readFrom == "" {
		readFrom = epoch
	}
	settings := []string{
		ydbdecl.AttributeImportant + " = " + strings.ToUpper(strconv.FormatBool(desired.Important)),
		ydbdecl.AttributeReadFrom + " = " + timestampLiteral(readFrom),
	}
	if len(desired.SupportedCodecs) > 0 {
		settings = append(settings, ydbdecl.AttributeSupportedCodecs+" = "+codecsLiteral(desired.SupportedCodecs))
	}
	if desired.AvailabilityPeriod != "" || previous.AvailabilityPeriod != "" {
		settings = append(settings, ydbdecl.AttributeAvailabilityPeriod+" = "+intervalLiteral(intervalSeconds(desired.AvailabilityPeriod, 0)))
	}
	return strings.Join(settings, ", ")
}

// epoch is the read_from YDB reports for a consumer that declared none.
const epoch = "1970-01-01T00:00:00Z"

// quoteName quotes an identifier or a path for YQL.
func quoteName(name string) string {
	return "`" + strings.ReplaceAll(strings.ReplaceAll(name, `\`, `\\`), "`", "\\`") + "`"
}

// quoteString writes a YQL string literal.
func quoteString(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), "'", `\'`) + "'"
}

// intervalLiteral writes a number of seconds as YDB's Interval literal.
func intervalLiteral(seconds uint64) string {
	return "Interval(" + quoteString(FormatSeconds(seconds)) + ")"
}

// timestampLiteral writes an RFC 3339 time as YDB's Timestamp literal.
func timestampLiteral(text string) string {
	return "Timestamp(" + quoteString(text) + ")"
}

// codecsLiteral writes a codec list as the string YDB takes, an empty list as
// the empty string, which takes any codec.
func codecsLiteral(codecs []string) string {
	lower := make([]string, len(codecs))
	for i, codec := range codecs {
		lower[i] = strings.ToLower(strings.TrimSpace(codec))
	}
	return quoteString(strings.Join(lower, ","))
}

func formatCount(count uint64) string { return strconv.FormatUint(count, 10) }

// Display names a topic by its path relative to the database root, unquoted,
// for messages.
func Display(schema, name string) string { return ydbdecl.TopicDisplay(schema, name) }

// consumerSubject names one consumer of a topic, for a refusal.
func consumerSubject(path, consumer string) string {
	return fmt.Sprintf("consumer %q of topic %s", consumer, path)
}
