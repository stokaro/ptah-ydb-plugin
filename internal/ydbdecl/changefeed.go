package ydbdecl

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// The attributes that declare a changefeed.
const (
	ChangefeedName                    = "name"
	AttributeMode                     = "mode"
	AttributeFormat                   = "format"
	AttributeVirtualTimestamps        = "virtual_timestamps"
	AttributeResolvedTimestamps       = "resolved_timestamps"
	AttributeInitialScan              = "initial_scan"
	AttributeUserSIDs                 = "user_sids"
	AttributeSchemaChanges            = "schema_changes"
	AttributeTopicMinActivePartitions = "topic_min_active_partitions"
	AttributeTopicAutoPartitioning    = "topic_auto_partitioning"
	AttributeFeedRetentionPeriod      = "retention_period"
)

func changefeedModes() []string {
	return []string{"KEYS_ONLY", "UPDATES", "NEW_IMAGE", "OLD_IMAGE", "NEW_AND_OLD_IMAGES"}
}

func changefeedFormats() []string { return []string{"JSON", "DEBEZIUM_JSON"} }

// Changefeed is a row table's changefeed as the host's spec encodes it.
type Changefeed struct {
	Name                     string     `json:"name"`
	Mode                     string     `json:"mode"`
	Format                   string     `json:"format"`
	VirtualTimestamps        bool       `json:"virtual_timestamps,omitempty"`
	ResolvedTimestamps       string     `json:"resolved_timestamps,omitempty"`
	InitialScan              bool       `json:"initial_scan,omitempty"`
	UserSIDs                 bool       `json:"user_sids,omitempty"`
	SchemaChanges            bool       `json:"schema_changes,omitempty"`
	TopicMinActivePartitions uint64     `json:"topic_min_active_partitions,omitempty"`
	TopicAutoPartitioning    bool       `json:"topic_auto_partitioning,omitempty"`
	RetentionPeriod          string     `json:"retention_period,omitempty"`
	Consumers                []Consumer `json:"consumers,omitempty"`
}

// ParseChangefeed reads one changefeed out of values, keyed by attribute
// name. The changefeed has no consumers.
func ParseChangefeed(values map[string]string) (Changefeed, error) {
	spec := Changefeed{Name: strings.TrimSpace(values[ChangefeedName])}
	if spec.Name == "" {
		return Changefeed{}, &DeclarationError{Attribute: ChangefeedName, Reason: "a changefeed needs a name"}
	}
	var err error
	if spec.Mode, err = feedChoice(values, AttributeMode, changefeedModes()); err != nil {
		return Changefeed{}, err
	}
	if spec.Format, err = feedFormat(values); err != nil {
		return Changefeed{}, err
	}
	switches := []struct {
		attribute string
		target    *bool
	}{
		{AttributeVirtualTimestamps, &spec.VirtualTimestamps},
		{AttributeInitialScan, &spec.InitialScan},
		{AttributeUserSIDs, &spec.UserSIDs},
		{AttributeSchemaChanges, &spec.SchemaChanges},
		{AttributeTopicAutoPartitioning, &spec.TopicAutoPartitioning},
	}
	for _, sw := range switches {
		if *sw.target, err = feedBoolean(values, sw.attribute); err != nil {
			return Changefeed{}, err
		}
	}
	if spec.ResolvedTimestamps, err = feedInterval(values, AttributeResolvedTimestamps); err != nil {
		return Changefeed{}, err
	}
	if spec.RetentionPeriod, err = feedInterval(values, AttributeFeedRetentionPeriod); err != nil {
		return Changefeed{}, err
	}
	if raw, ok := present(values, AttributeTopicMinActivePartitions); ok {
		count, parseErr := strconv.ParseUint(raw, 10, 64)
		if parseErr != nil || count == 0 {
			return Changefeed{}, &DeclarationError{Attribute: AttributeTopicMinActivePartitions, Value: raw,
				Reason: "takes a count of at least 1 (`topic_min_active_partitions must be greater than 0`)"}
		}
		spec.TopicMinActivePartitions = count
	}
	return spec, nil
}

func feedChoice(values map[string]string, attribute string, allowed []string) (string, error) {
	raw, _ := present(values, attribute)
	upper := strings.ToUpper(raw)
	if !slices.Contains(allowed, upper) {
		return "", &DeclarationError{Attribute: attribute, Value: raw, Reason: "takes one of " + strings.Join(allowed, ", ")}
	}
	return upper, nil
}

func feedFormat(values map[string]string) (string, error) {
	raw, _ := present(values, AttributeFormat)
	if strings.EqualFold(raw, "DYNAMODB_STREAMS_JSON") {
		return "", &DeclarationError{Attribute: AttributeFormat, Value: raw,
			Reason: "YDB writes DYNAMODB_STREAMS_JSON only for a document table, which Ptah does not create " +
				"(`DYNAMODB_STREAMS_JSON format incompatible with non-document table`)"}
	}
	return feedChoice(values, AttributeFormat, changefeedFormats())
}

func feedBoolean(values map[string]string, attribute string) (bool, error) {
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
		return false, &DeclarationError{Attribute: attribute, Value: raw, Reason: "takes true or false"}
	}
}

// FeedSeconds reads one of a changefeed's intervals as whole seconds, in any
// case. YDB's Interval grammar holds weeks, days, hours, minutes and seconds,
// and a changefeed refuses an interval of no length, which a TTL takes.
func FeedSeconds(text string) (uint64, error) {
	seconds, err := IntervalSeconds(strings.ToUpper(strings.TrimSpace(text)))
	if err != nil {
		return 0, err
	}
	if seconds == 0 {
		return 0, fmt.Errorf("interval %q is no time at all, and YDB takes only a positive interval", text)
	}
	return seconds, nil
}

// FormatInterval writes seconds as the shortest ISO 8601 duration of days,
// hours, minutes and seconds, as the host writes a changefeed's interval.
func FormatInterval(seconds uint64) string { return formatInterval(seconds) }

func feedInterval(values map[string]string, attribute string) (string, error) {
	raw, ok := present(values, attribute)
	if !ok {
		return "", nil
	}
	seconds, err := IntervalSeconds(strings.ToUpper(strings.TrimSpace(raw)))
	if err == nil && seconds == 0 {
		err = fmt.Errorf("interval %q is no time at all, and YDB takes only a positive interval", raw)
	}
	if err != nil {
		return "", &DeclarationError{Attribute: attribute, Value: raw, Reason: err.Error()}
	}
	return raw, nil
}
