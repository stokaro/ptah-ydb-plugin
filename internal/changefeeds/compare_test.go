package changefeeds_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// base is a changefeed as a declaration states it; each row below changes one
// thing about a copy of it.
func base() changefeeds.Feed {
	return changefeeds.Feed{Changefeed: ydbdecl.Changefeed{
		Name: "feed", Mode: "UPDATES", Format: "JSON", RetentionPeriod: "PT12H",
		Consumers: []ydbdecl.Consumer{{Name: "audit", Important: true, SupportedCodecs: []string{"raw", "gzip"}}},
	}}
}

// TestEqual_ReadsAsYDBKeeps holds the spellings YDB keeps as one value equal
// to each other, so a database read back compares equal to its declaration.
func TestEqual_ReadsAsYDBKeeps(t *testing.T) {
	read := base()
	read.Mode, read.Format = "updates", "json"
	read.RetentionPeriod = "PT720M"
	read.Consumers = []ydbdecl.Consumer{{Name: "audit", Important: true, SupportedCodecs: []string{"GZIP", "raw"}, ReadFrom: "1970-01-01T00:00:00Z"}}
	defaultRetention := base()
	defaultRetention.RetentionPeriod = ""
	readDefault := base()
	readDefault.RetentionPeriod = "P1D"
	unknownPartitions := base()
	unknownPartitions.TopicMinActivePartitions = 3
	tests := []struct {
		name             string
		desired, current changefeeds.Feed
	}{
		{name: "case, interval spelling, codec order and the epoch", desired: base(), current: read},
		{name: "no retention and YDB's 24 hours", desired: defaultRetention, current: readDefault},
		{name: "a partition count the declaration does not name", desired: base(), current: unknownPartitions},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(changefeeds.Equal(test.desired, test.current), qt.IsTrue)
			c.Assert(changefeeds.Equal(test.current, test.desired), qt.IsTrue)
		})
	}
}

// TestRecreated_HappyPath pins each option of ADD CHANGEFEED as one whose
// change drops and adds the changefeed.
func TestRecreated_HappyPath(t *testing.T) {
	change := func(edit func(*changefeeds.Feed)) changefeeds.Feed {
		spec := base()
		edit(&spec)
		return spec
	}
	tests := []struct {
		name    string
		current changefeeds.Feed
	}{
		{name: "mode", current: change(func(s *changefeeds.Feed) { s.Mode = "KEYS_ONLY" })},
		{name: "format", current: change(func(s *changefeeds.Feed) { s.Format = "DEBEZIUM_JSON" })},
		{name: "virtual timestamps", current: change(func(s *changefeeds.Feed) { s.VirtualTimestamps = true })},
		{name: "resolved timestamps", current: change(func(s *changefeeds.Feed) { s.ResolvedTimestamps = "PT1S" })},
		{name: "initial scan", current: change(func(s *changefeeds.Feed) { s.InitialScan = true })},
		{name: "user SIDs", current: change(func(s *changefeeds.Feed) { s.UserSIDs = true })},
		{name: "schema changes", current: change(func(s *changefeeds.Feed) { s.SchemaChanges = true })},
		{name: "auto partitioning", current: change(func(s *changefeeds.Feed) { s.TopicAutoPartitioning = true })},
		{name: "disabled", current: change(func(s *changefeeds.Feed) { s.Disabled = true })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(changefeeds.Recreated(base(), test.current), qt.IsTrue)
			c.Assert(changefeeds.Equal(base(), test.current), qt.IsFalse)
		})
	}
}

// TestRecreated_PartitionCountBothSidesName holds a partition count both sides
// name as an option like the others.
func TestRecreated_PartitionCountBothSidesName(t *testing.T) {
	c := qt.New(t)
	desired, current := base(), base()
	desired.TopicMinActivePartitions, current.TopicMinActivePartitions = 2, 3
	c.Assert(changefeeds.Recreated(desired, current), qt.IsTrue)
}

// TestTopicChanged_HappyPath pins the retention and each consumer setting as
// changes of the topic rather than of the changefeed.
func TestTopicChanged_HappyPath(t *testing.T) {
	consumer := func(edit func(*ydbdecl.Consumer)) changefeeds.Feed {
		spec := base()
		spec.Consumers = []ydbdecl.Consumer{spec.Consumers[0]}
		edit(&spec.Consumers[0])
		return spec
	}
	retention := base()
	retention.RetentionPeriod = ""
	added := base()
	added.Consumers = append(added.Consumers, ydbdecl.Consumer{Name: "other"})
	renamed := base()
	renamed.Consumers = []ydbdecl.Consumer{{Name: "audit2", Important: true, SupportedCodecs: []string{"raw", "gzip"}}}
	tests := []struct {
		name    string
		current changefeeds.Feed
	}{
		{name: "retention", current: retention},
		{name: "a consumer added", current: added},
		{name: "a consumer under another name", current: renamed},
		{name: "important", current: consumer(func(c *ydbdecl.Consumer) { c.Important = false })},
		{name: "read_from", current: consumer(func(c *ydbdecl.Consumer) { c.ReadFrom = "2026-01-01T00:00:00Z" })},
		{name: "codecs", current: consumer(func(c *ydbdecl.Consumer) { c.SupportedCodecs = []string{"raw"} })},
		{name: "availability", current: consumer(func(c *ydbdecl.Consumer) { c.AvailabilityPeriod = "PT1H" })},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(changefeeds.TopicChanged(base(), test.current), qt.IsTrue)
			c.Assert(changefeeds.Recreated(base(), test.current), qt.IsFalse)
		})
	}
}

// TestConsumerPositionsLost_HappyPath names the consumers whose positions a
// topic change discards: the ones dropped, and the ones dropped and added
// again because their codecs are emptied.
func TestConsumerPositionsLost_HappyPath(t *testing.T) {
	current := base()
	current.Consumers = []ydbdecl.Consumer{{Name: "audit", SupportedCodecs: []string{"raw"}}, {Name: "billing"}, {Name: "kept", Important: true}}
	desired := base()
	desired.Consumers = []ydbdecl.Consumer{{Name: "audit"}, {Name: "kept"}, {Name: "fresh"}}
	c := qt.New(t)
	c.Assert(changefeeds.ConsumerPositionsLost(desired, current), qt.DeepEquals, []string{"audit", "billing"})
	c.Assert(changefeeds.ConsumersChanged(desired, current), qt.IsTrue)
	c.Assert(changefeeds.RetentionChanged(desired, current), qt.IsFalse)
}
