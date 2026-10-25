package topics_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// YDB keeps every partition a topic has and keeps auto-partitioning once it
// is on, so a declaration that asks for either is refused with the host's
// words rather than planned as a drop that loses every message.
func TestChangeRefusal_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name             string
		desired, current ydbdecl.Topic
		want             string
	}{
		{"fewer partitions", ydbdecl.Topic{MinActivePartitions: 1}, ydbdecl.Topic{MinActivePartitions: 3},
			"topic app/events: it has 3 partitions and is declared with 1, and YDB never removes a partition \\(`Invalid total groups count specified`\\); drop the topic and create it again to lower the count"},
		{"auto-partitioning switched off", ydbdecl.Topic{}, ydbdecl.Topic{AutoPartitioningStrategy: "scale_up", MaxActivePartitions: 4},
			"topic app/events: its auto-partitioning is scale_up and is declared disabled, and YDB does not disable auto-partitioning once it is on \\(`Can't disable auto partitioning.`\\); declare auto_partitioning_strategy paused to stop it"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(topics.ChangeRefusal("app", "events", test.desired, test.current), qt.ErrorMatches, test.want)
		})
	}
}

func TestChangeRefusal_HappyPath(t *testing.T) {
	c := qt.New(t)
	c.Assert(topics.ChangeRefusal("app", "events", ydbdecl.Topic{MinActivePartitions: 4}, ydbdecl.Topic{MinActivePartitions: 3}), qt.IsNil)
	c.Assert(topics.ChangeRefusal("app", "events", ydbdecl.Topic{AutoPartitioningStrategy: "paused"}, ydbdecl.Topic{AutoPartitioningStrategy: "scale_up"}), qt.IsNil)
}

// A target holds the capabilities a declared topic needs, or the topic is
// refused naming the capability, as the host refuses it.
func TestCheck_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name         string
		spec         ydbdecl.Topic
		capabilities map[string]bool
		want         string
	}{
		{"no topics", ydbdecl.Topic{}, map[string]bool{}, "topic app/events, which requires target capability topics, unavailable on this ydb target"},
		{"an availability period on 25.3", ydbdecl.Topic{Consumers: []ydbdecl.Consumer{{Name: "c", AvailabilityPeriod: "PT2H"}}}, map[string]bool{"topics": true},
			`consumer "c" of topic app/events takes availability_period, which requires target capability topic_consumer_availability_period, unavailable on this ydb target`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(topics.Check("app", "events", test.spec, test.capabilities), qt.ErrorMatches, test.want)
		})
	}
}

func TestCheck_HappyPath(t *testing.T) {
	c := qt.New(t)
	spec := ydbdecl.Topic{Consumers: []ydbdecl.Consumer{{Name: "c", AvailabilityPeriod: "PT2H"}}}
	c.Assert(topics.Check("app", "events", spec, map[string]bool{"topics": true, "topic_consumer_availability_period": true}), qt.IsNil)
}
