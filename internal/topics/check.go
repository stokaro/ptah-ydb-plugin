package topics

import (
	"fmt"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// The capability keys a topic's statements need, as the host names them.
const (
	// CapabilityTopics is a target that keeps standalone topics.
	CapabilityTopics = "topics"
	// CapabilityAvailabilityPeriod is a target whose consumers take
	// availability_period (YDB 25.4 and later).
	CapabilityAvailabilityPeriod = "topic_consumer_availability_period"
)

// Check reports why a topic declared as spec cannot be created or changed on
// a target holding capabilities, or nil when it can. An absent capability is
// one the target was not shown to hold.
func Check(schema, name string, spec ydbdecl.Topic, capabilities map[string]bool) error {
	path := Display(schema, name)
	if !capabilities[CapabilityTopics] {
		return capabilityRefusal("topic "+path, CapabilityTopics)
	}
	if err := ydbdecl.CheckTopic(schema, name, spec); err != nil {
		return fmt.Errorf("topic %s: %w", path, err)
	}
	for _, consumer := range spec.Consumers {
		if consumer.AvailabilityPeriod != "" && !capabilities[CapabilityAvailabilityPeriod] {
			return capabilityRefusal(consumerSubject(path, consumer.Name)+" takes availability_period", CapabilityAvailabilityPeriod)
		}
	}
	return nil
}

func capabilityRefusal(subject, key string) error {
	return fmt.Errorf("%s, which requires target capability %s, unavailable on this ydb target", subject, key)
}

// ChangeRefusal reports why YDB cannot move a topic from current to desired
// in place, or nil when it can. Measured on 25.1.4.7 and 26.2.1.14: a topic
// keeps every partition it has (`Invalid total groups count specified: 1 vs 2
// (current)`), and keeps auto-partitioning once it is enabled (`Can't disable
// auto partitioning.`). Either change drops the topic and every message in it,
// so it is refused rather than planned.
func ChangeRefusal(schema, name string, desired, current ydbdecl.Topic) error {
	want, have := Resolve(desired), Resolve(current)
	subject := "topic " + Display(schema, name)
	switch {
	case want.MinActivePartitions < have.MinActivePartitions:
		return fmt.Errorf("%s: it has %d partitions and is declared with %d, and YDB never removes a partition "+
			"(`Invalid total groups count specified`); drop the topic and create it again to lower the count",
			subject, have.MinActivePartitions, want.MinActivePartitions)
	case have.AutoPartitioned() && !want.AutoPartitioned():
		return fmt.Errorf("%s: its auto-partitioning is %s and is declared disabled, and YDB does not disable "+
			"auto-partitioning once it is on (`Can't disable auto partitioning.`); declare %s %s to stop it",
			subject, have.Strategy, ydbdecl.AttributeStrategy, ydbdecl.StrategyPaused)
	default:
		return nil
	}
}
