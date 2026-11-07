package ydbdecl_test

import (
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// The parse cases are the host's dialect/ydb/ydbtopic parse cases. They read
// ParseTopic and ParseConsumer directly: a topic source document reaches the
// model through the topic's intent, which folds codecs and times again, so a
// row there could not see the parse's own normalization.

// A topic's settings read with a strategy in any case, a codec list with
// spaces and upper case folded, the custom codec, the shaping settings of a
// paused topic, and a maximum equal to the minimum. Ported from the host's
// TestParseTopic_HappyPath.
func TestParseTopic_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		want   ydbdecl.Topic
	}{
		{name: "nothing declared", values: map[string]string{"name": "events"}},
		{name: "every setting",
			values: map[string]string{"min_active_partitions": "2", "max_active_partitions": "6", "auto_partitioning_strategy": "Scale_Up",
				"auto_partitioning_up_utilization_percent": "70", "auto_partitioning_down_utilization_percent": "10",
				"auto_partitioning_stabilization_window": "PT2M", "retention_period": "P1DT12H", "partition_write_speed_bytes_per_second": "2097152",
				"partition_write_burst_bytes": "3145728", "supported_codecs": " RAW, gzip ,custom"},
			want: ydbdecl.Topic{MinActivePartitions: 2, MaxActivePartitions: 6, AutoPartitioningStrategy: "scale_up", AutoPartitioningUpUtilizationPercent: 70,
				AutoPartitioningDownUtilizationPercent: 10, AutoPartitioningStabilizationWindow: "PT2M", RetentionPeriod: "P1DT12H",
				PartitionWriteSpeedBytesPerSecond: 2097152, PartitionWriteBurstBytes: 3145728, SupportedCodecs: []string{"raw", "gzip", "custom"}}},
		{name: "paused keeps the shaping settings", values: map[string]string{"auto_partitioning_strategy": "paused", "max_active_partitions": "3"},
			want: ydbdecl.Topic{AutoPartitioningStrategy: "paused", MaxActivePartitions: 3}},
		{name: "a maximum equal to the minimum",
			values: map[string]string{"auto_partitioning_strategy": "scale_up_and_down", "min_active_partitions": "3", "max_active_partitions": "3"},
			want:   ydbdecl.Topic{AutoPartitioningStrategy: "scale_up_and_down", MinActivePartitions: 3, MaxActivePartitions: 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbdecl.ParseTopic(test.values)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
		})
	}
}

// A consumer reads with its name trimmed, important in any case, a read_from
// in another zone in UTC, a read_from at the epoch as none, and codecs in
// lower case. Ported from the host's TestParseConsumer_HappyPath.
func TestParseConsumer_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		want   ydbdecl.Consumer
	}{
		{name: "a name alone", values: map[string]string{"name": " billing ", "topic": "events"}, want: ydbdecl.Consumer{Name: "billing"}},
		{name: "important", values: map[string]string{"name": "billing", "important": "TRUE"}, want: ydbdecl.Consumer{Name: "billing", Important: true}},
		{name: "a read_from in another zone", values: map[string]string{"name": "audit", "read_from": "2026-01-01T03:00:00+03:00"},
			want: ydbdecl.Consumer{Name: "audit", ReadFrom: "2026-01-01T00:00:00Z"}},
		{name: "read_from at the epoch is no read_from", values: map[string]string{"name": "audit", "read_from": "1970-01-01T00:00:00Z"},
			want: ydbdecl.Consumer{Name: "audit"}},
		{name: "codecs and an availability period", values: map[string]string{"name": "audit", "supported_codecs": "zstd,raw", "availability_period": "PT2H"},
			want: ydbdecl.Consumer{Name: "audit", SupportedCodecs: []string{"zstd", "raw"}, AvailabilityPeriod: "PT2H"}},
		{name: "codecs in lower case", values: map[string]string{"name": "audit", "supported_codecs": "RAW, gzip"},
			want: ydbdecl.Consumer{Name: "audit", SupportedCodecs: []string{"raw", "gzip"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbdecl.ParseConsumer(test.values)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
		})
	}
}

// A consumer YDB would not keep as written is refused with the host's words.
// Ported from the host's TestParseConsumer_FailurePath.
func TestParseConsumer_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		values map[string]string
		want   string
	}{
		{name: "no name", values: map[string]string{"name": " "}, want: "invalid name: a consumer needs a name"},
		{name: "a slash in the name", values: map[string]string{"name": "a/b"}, want: `invalid name "a/b": a consumer's name cannot hold a slash`},
		{name: "important that is no boolean", values: map[string]string{"name": "c", "important": "yes"}, want: `invalid important "yes": takes true or false`},
		{name: "a read_from that is no time", values: map[string]string{"name": "c", "read_from": "2026-01-01"},
			want: `invalid read_from "2026-01-01": takes an RFC 3339 time such as 2026-01-01T00:00:00Z`},
		{name: "a read_from with a fraction", values: map[string]string{"name": "c", "read_from": "2026-01-01T00:00:00.5Z"},
			want: `invalid read_from "2026-01-01T00:00:00.5Z": YDB keeps whole seconds, and drops the fraction of this one`},
		{name: "an unknown codec", values: map[string]string{"name": "c", "supported_codecs": "snappy"},
			want: `invalid supported_codecs "snappy": takes a comma-separated list of raw, gzip, lzop, zstd, custom; YDB keeps a list naming any other codec as no list at all`},
		{name: "a codec twice", values: map[string]string{"name": "c", "supported_codecs": "raw,RAW"}, want: `invalid supported_codecs "raw,RAW": names codec raw twice`},
		{name: "important with an availability period", values: map[string]string{"name": "c", "important": "true", "availability_period": "PT1H"},
			want: `invalid availability_period "PT1H": YDB keeps every unread record for an important consumer, so it takes no availability period as well ` +
				"(`has both an important flag and a limited availability_period, which are mutually exclusive`)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbdecl.ParseConsumer(test.values)
			c.Assert(err, qt.ErrorMatches, regexp.QuoteMeta(test.want))
			c.Assert(got, qt.DeepEquals, ydbdecl.Consumer{})
		})
	}
}

// A topic's interval is a positive whole number of seconds: a negative one,
// a fraction of a second and a month are refused. Ported from the host's
// TestSeconds_FailurePath.
func TestTopicSeconds_FailurePath(t *testing.T) {
	for _, test := range []struct {
		text string
		want string
	}{
		{text: "-PT1S", want: "is no positive time, and YDB takes only a positive interval"},
		{text: "PT0.5S", want: "YDB keeps whole seconds, and drops the fraction of this one"},
		{text: "P1M", want: "takes an ISO 8601 duration such as PT12H or P1D"},
	} {
		t.Run(test.text, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbdecl.TopicSeconds(test.text)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(got, qt.Equals, uint64(0))
		})
	}
}
