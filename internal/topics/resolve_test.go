package topics_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// A declaration resolves to what YDB gives a new topic for each setting it
// leaves out: the burst follows the write speed, a topic created with a
// strategy splits at 90%, merges at 30%, waits five minutes and keeps a
// maximum of its minimum, a strategy reads in any case, codecs read as a
// set, and a disabled topic keeps no shaping setting. Ported from the host's
// TestResolve.
func TestResolve(t *testing.T) {
	for _, test := range []struct {
		name string
		spec ydbdecl.Topic
		want topics.Settings
	}{
		{name: "nothing declared",
			want: topics.Settings{MinActivePartitions: 1, Strategy: "disabled", RetentionSeconds: 86400, WriteSpeed: 1048576, WriteBurst: 1048576}},
		{name: "a write speed without a burst", spec: ydbdecl.Topic{PartitionWriteSpeedBytesPerSecond: 4194304},
			want: topics.Settings{MinActivePartitions: 1, Strategy: "disabled", RetentionSeconds: 86400, WriteSpeed: 4194304, WriteBurst: 4194304}},
		{name: "a strategy without thresholds or a maximum", spec: ydbdecl.Topic{MinActivePartitions: 3, AutoPartitioningStrategy: "Scale_Up"},
			want: topics.Settings{MinActivePartitions: 3, Strategy: "scale_up", MaxActivePartitions: 3, UpUtilizationPercent: 90, DownUtilizationPercent: 30,
				StabilizationWindowSeconds: 300, RetentionSeconds: 86400, WriteSpeed: 1048576, WriteBurst: 1048576}},
		{name: "every setting",
			spec: ydbdecl.Topic{MinActivePartitions: 2, MaxActivePartitions: 6, AutoPartitioningStrategy: "paused", AutoPartitioningUpUtilizationPercent: 70,
				AutoPartitioningDownUtilizationPercent: 10, AutoPartitioningStabilizationWindow: "PT2M", RetentionPeriod: "PT36H",
				PartitionWriteSpeedBytesPerSecond: 2097152, PartitionWriteBurstBytes: 3145728, SupportedCodecs: []string{"GZIP", "raw", "gzip"}},
			want: topics.Settings{MinActivePartitions: 2, Strategy: "paused", MaxActivePartitions: 6, UpUtilizationPercent: 70, DownUtilizationPercent: 10,
				StabilizationWindowSeconds: 120, RetentionSeconds: 129600, WriteSpeed: 2097152, WriteBurst: 3145728, Codecs: []string{"gzip", "raw"}}},
		{name: "the shaping settings of a disabled topic are none", spec: ydbdecl.Topic{MaxActivePartitions: 5, AutoPartitioningUpUtilizationPercent: 70},
			want: topics.Settings{MinActivePartitions: 1, Strategy: "disabled", RetentionSeconds: 86400, WriteSpeed: 1048576, WriteBurst: 1048576}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(topics.Resolve(test.spec), qt.DeepEquals, test.want)
		})
	}
}

// Two descriptions of one topic compare as YDB keeps it. Each row differs
// from the read of a topic created without settings in one setting, or in
// how it is spelled. Ported from the host's TestSettingsEqual.
func TestSettingsEqual(t *testing.T) {
	read := ydbdecl.Topic{MinActivePartitions: 1, AutoPartitioningStrategy: "disabled", RetentionPeriod: "P1D",
		PartitionWriteSpeedBytesPerSecond: 1048576, PartitionWriteBurstBytes: 1048576}
	for _, test := range []struct {
		name     string
		declared ydbdecl.Topic
		want     bool
	}{
		{name: "nothing declared", want: true},
		{name: "each default named", declared: read, want: true},
		{name: "the retention spelled in hours", declared: ydbdecl.Topic{RetentionPeriod: "PT24H"}, want: true},
		{name: "another retention", declared: ydbdecl.Topic{RetentionPeriod: "PT25H"}},
		{name: "another partition count", declared: ydbdecl.Topic{MinActivePartitions: 2}},
		{name: "a strategy", declared: ydbdecl.Topic{AutoPartitioningStrategy: "paused"}},
		{name: "another write speed", declared: ydbdecl.Topic{PartitionWriteSpeedBytesPerSecond: 2097152}},
		{name: "another burst", declared: ydbdecl.Topic{PartitionWriteBurstBytes: 2097152}},
		{name: "codecs", declared: ydbdecl.Topic{SupportedCodecs: []string{"raw"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(topics.SettingsEqual(test.declared, read), qt.Equals, test.want)
			c.Assert(topics.Equal(test.declared, read), qt.Equals, test.want)
		})
	}
}

// The thresholds of an auto-partitioned topic compare even where both sides
// agree on the strategy: a topic given its strategy after it was created
// keeps 80% and 20%, and a declaration leaving the thresholds out asks for
// the 90% and 30% a topic created with a strategy gets. Ported from the
// host's TestSettingsEqual_ThresholdsOfAnAutoPartitionedTopic.
func TestSettingsEqualComparesTheThresholdsOfAnAutoPartitionedTopic(t *testing.T) {
	given := ydbdecl.Topic{MinActivePartitions: 1, MaxActivePartitions: 4, AutoPartitioningStrategy: "scale_up",
		AutoPartitioningUpUtilizationPercent: 80, AutoPartitioningDownUtilizationPercent: 20, AutoPartitioningStabilizationWindow: "PT5M",
		RetentionPeriod: "P1D", PartitionWriteSpeedBytesPerSecond: 1048576, PartitionWriteBurstBytes: 1048576}
	for _, test := range []struct {
		name     string
		declared ydbdecl.Topic
		want     bool
	}{
		{name: "the thresholds left out", declared: ydbdecl.Topic{MaxActivePartitions: 4, AutoPartitioningStrategy: "scale_up"}},
		{name: "the thresholds the topic keeps", declared: ydbdecl.Topic{MaxActivePartitions: 4, AutoPartitioningStrategy: "scale_up",
			AutoPartitioningUpUtilizationPercent: 80, AutoPartitioningDownUtilizationPercent: 20}, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(topics.SettingsEqual(test.declared, given), qt.Equals, test.want)
		})
	}
}

// Consumers compare by name: one only the declaration has is added, one only
// the topic has is removed, a read_from of the same instant in another zone
// and codecs differing only in case and order are no change, a widened codec
// list is a change, and a codec list emptied is a restart. Consumers lists
// them added, removed, changed, restarted. Ported from the host's TestCompare.
func TestCompare(t *testing.T) {
	c := testcheck.New(t)
	current := ydbdecl.Topic{Consumers: []ydbdecl.Consumer{
		{Name: "gone"},
		{Name: "kept", Important: true},
		{Name: "same", ReadFrom: "2026-01-01T00:00:00Z", SupportedCodecs: []string{"raw", "gzip"}},
		{Name: "narrowed", SupportedCodecs: []string{"raw"}},
		{Name: "widened"},
	}}
	desired := ydbdecl.Topic{Consumers: []ydbdecl.Consumer{
		{Name: "fresh"},
		{Name: "kept"},
		{Name: "same", ReadFrom: "2026-01-01T03:00:00+03:00", SupportedCodecs: []string{"GZIP", "raw"}},
		{Name: "narrowed"},
		{Name: "widened", SupportedCodecs: []string{"zstd"}},
	}}

	changes := topics.Compare(desired, current)

	c.Assert(changes, qt.DeepEquals, topics.ConsumerChanges{Added: []string{"fresh"}, Removed: []string{"gone"}, Changed: []string{"kept", "widened"},
		Restarted: []string{"narrowed"}})
	c.Assert(changes.Consumers(), qt.DeepEquals, []string{"fresh", "gone", "kept", "widened", "narrowed"})
	c.Assert(topics.Equal(desired, current), qt.IsFalse)
}

// A consumer compares as YDB keeps it: a read_from at the epoch is none, an
// availability period compares by the seconds it denotes, and a read_from
// one second apart is another. Ported from the host's TestConsumerEqual.
func TestConsumerEqual(t *testing.T) {
	for _, test := range []struct {
		name string
		a, b ydbdecl.Consumer
		want bool
	}{
		{name: "read_from at the epoch", a: ydbdecl.Consumer{Name: "c"}, b: ydbdecl.Consumer{Name: "c", ReadFrom: "1970-01-01T00:00:00Z"}, want: true},
		{name: "one period in two spellings", a: ydbdecl.Consumer{Name: "c", AvailabilityPeriod: "PT48H"},
			b: ydbdecl.Consumer{Name: "c", AvailabilityPeriod: "P2D"}, want: true},
		{name: "another period", a: ydbdecl.Consumer{Name: "c", AvailabilityPeriod: "PT1H"}, b: ydbdecl.Consumer{Name: "c"}},
		{name: "important", a: ydbdecl.Consumer{Name: "c", Important: true}, b: ydbdecl.Consumer{Name: "c"}},
		{name: "another read_from", a: ydbdecl.Consumer{Name: "c", ReadFrom: "2026-01-01T00:00:01Z"},
			b: ydbdecl.Consumer{Name: "c", ReadFrom: "2026-01-01T00:00:00Z"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(topics.ConsumerEqual(test.a, test.b), qt.Equals, test.want)
		})
	}
}
