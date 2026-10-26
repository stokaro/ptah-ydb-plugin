package changefeeds_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// The statement cases are the host's internal/ydbchangefeed cases, with the
// table written as the path the provider runs them against.

func TestAddStatements_HappyPath(t *testing.T) {
	tests := []struct {
		name  string
		table string
		spec  ydbdecl.Changefeed
		want  []string
	}{
		{
			name: "the two options YDB requires", table: "items",
			spec: ydbdecl.Changefeed{Name: "updates", Mode: "updates", Format: "json"},
			want: []string{"ALTER TABLE `items` ADD CHANGEFEED `updates` WITH (MODE = 'UPDATES', FORMAT = 'JSON');"},
		},
		{
			name: "every option and three consumers, in a directory", table: "/local/app/items",
			spec: ydbdecl.Changefeed{
				Name: "feed", Mode: "NEW_AND_OLD_IMAGES", Format: "JSON", VirtualTimestamps: true,
				ResolvedTimestamps: "PT90M", RetentionPeriod: "pt12h", InitialScan: true, UserSIDs: true,
				SchemaChanges: true, TopicAutoPartitioning: true, TopicMinActivePartitions: 2,
				Consumers: []ydbdecl.Consumer{
					{Name: "audit"},
					{Name: "billing", Important: true, ReadFrom: "2026-01-01T00:00:00Z", SupportedCodecs: []string{"raw", "GZIP"}},
					{Name: "late", AvailabilityPeriod: "P1D"},
				},
			},
			want: []string{
				"ALTER TABLE `/local/app/items` ADD CHANGEFEED `feed` WITH (MODE = 'NEW_AND_OLD_IMAGES', " +
					"FORMAT = 'JSON', VIRTUAL_TIMESTAMPS = TRUE, RESOLVED_TIMESTAMPS = Interval('PT1H30M'), " +
					"RETENTION_PERIOD = Interval('PT12H'), INITIAL_SCAN = TRUE, USER_SIDS = TRUE, SCHEMA_CHANGES = TRUE, " +
					"TOPIC_AUTO_PARTITIONING = 'ENABLED', TOPIC_MIN_ACTIVE_PARTITIONS = 2);",
				"ALTER TOPIC `/local/app/items/feed` ADD CONSUMER `audit`;",
				"ALTER TOPIC `/local/app/items/feed` ADD CONSUMER `billing` WITH (important = TRUE, " +
					"read_from = Timestamp('2026-01-01T00:00:00Z'), supported_codecs = 'raw,gzip');",
				"ALTER TOPIC `/local/app/items/feed` ADD CONSUMER `late` WITH (availability_period = Interval('P1D'));",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(changefeeds.AddStatements(test.table, test.spec), qt.DeepEquals, test.want)
		})
	}
}

func TestDropStatement_HappyPath(t *testing.T) {
	c := qt.New(t)
	c.Assert(changefeeds.DropStatement("/local/app/items", "feed"), qt.Equals, "ALTER TABLE `/local/app/items` DROP CHANGEFEED `feed`;")
}

func TestTopicStatements_HappyPath(t *testing.T) {
	spec := func(retention string, consumers ...ydbdecl.Consumer) ydbdecl.Changefeed {
		return ydbdecl.Changefeed{Name: "feed", Mode: "UPDATES", Format: "JSON", RetentionPeriod: retention, Consumers: consumers}
	}
	tests := []struct {
		name              string
		desired, previous ydbdecl.Changefeed
		want              []string
		wantRestarted     []string
	}{
		{
			name: "a retention set", desired: spec("PT6H"), previous: spec(""),
			want: []string{"ALTER TOPIC `items/feed` SET (retention_period = Interval('PT6H'));"},
		},
		{
			name: "a retention removed is set to 24 hours, never reset", desired: spec(""), previous: spec("PT6H"),
			want: []string{"ALTER TOPIC `items/feed` SET (retention_period = Interval('P1D'));"},
		},
		{
			name:    "a consumer added and one dropped",
			desired: spec("", ydbdecl.Consumer{Name: "new"}), previous: spec("", ydbdecl.Consumer{Name: "old"}),
			want: []string{
				"ALTER TOPIC `items/feed` DROP CONSUMER `old`;",
				"ALTER TOPIC `items/feed` ADD CONSUMER `new`;",
			},
		},
		{
			// The statement names important and read_from although only the
			// codecs differ, so its outcome does not depend on what the
			// consumer held.
			name:     "a consumer changed in place",
			desired:  spec("", ydbdecl.Consumer{Name: "c", SupportedCodecs: []string{"raw"}}),
			previous: spec("", ydbdecl.Consumer{Name: "c", SupportedCodecs: []string{"raw", "gzip"}}),
			want: []string{"ALTER TOPIC `items/feed` ALTER CONSUMER `c` SET (important = FALSE, " +
				"read_from = Timestamp('1970-01-01T00:00:00Z'), supported_codecs = 'raw');"},
		},
		{
			name:     "an availability period removed is set to zero",
			desired:  spec("", ydbdecl.Consumer{Name: "c", Important: true}),
			previous: spec("", ydbdecl.Consumer{Name: "c", AvailabilityPeriod: "PT1H"}),
			want: []string{"ALTER TOPIC `items/feed` ALTER CONSUMER `c` SET (important = TRUE, " +
				"read_from = Timestamp('1970-01-01T00:00:00Z'), availability_period = Interval('PT0S'));"},
		},
		{
			name:     "a consumer's codecs removed drops and adds it",
			desired:  spec("", ydbdecl.Consumer{Name: "c"}),
			previous: spec("", ydbdecl.Consumer{Name: "c", SupportedCodecs: []string{"raw"}}),
			want: []string{
				"ALTER TOPIC `items/feed` DROP CONSUMER `c`;",
				"ALTER TOPIC `items/feed` ADD CONSUMER `c`;",
			},
			wantRestarted: []string{"c"},
		},
		{
			name:    "nothing to change",
			desired: spec("P1D", ydbdecl.Consumer{Name: "c"}), previous: spec("", ydbdecl.Consumer{Name: "c"}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, restarted := changefeeds.TopicStatements("items", test.desired, test.previous)
			c.Assert(got, qt.DeepEquals, test.want)
			c.Assert(restarted, qt.DeepEquals, test.wantRestarted)
		})
	}
}

func TestTopicPath_HappyPath(t *testing.T) {
	c := qt.New(t)
	c.Assert(changefeeds.TopicPath("items", "feed"), qt.Equals, "`items/feed`")
	c.Assert(changefeeds.TopicPath("/local/app/items", "feed"), qt.Equals, "`/local/app/items/feed`")
}
