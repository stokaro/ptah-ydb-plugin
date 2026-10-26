package changefeeds_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// line returns the changefeed capabilities a session reports for one YDB
// release line, as the provider's session derives them from the version.
func line(major, minor int) map[string]bool {
	at := func(m, n int) bool { return major > m || major == m && minor >= n }
	return map[string]bool{
		changefeeds.CapabilityChangefeeds:           true,
		changefeeds.CapabilityUserSIDs:              at(26, 1),
		changefeeds.CapabilitySchemaChanges:         at(25, 3),
		changefeeds.CapabilityTopicAutoPartitioning: at(25, 2),
		"topic_consumer_availability_period":        at(25, 4),
	}
}

// TestCheck_HappyPath holds that a changefeed the target can write passes, the
// options a line lacks included where the line has them.
func TestCheck_HappyPath(t *testing.T) {
	tests := []struct {
		name string
		spec changefeeds.Feed
		caps map[string]bool
	}{
		{name: "the plainest changefeed on the oldest line", caps: line(25, 1),
			spec: changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON"}}},
		{name: "every option on the newest line", caps: line(26, 2),
			spec: changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "f", Mode: "NEW_IMAGE", Format: "JSON", UserSIDs: true,
				SchemaChanges: true, TopicAutoPartitioning: true, ResolvedTimestamps: "PT1S", RetentionPeriod: "P31D",
				Consumers: []ydbdecl.Consumer{{Name: "a", AvailabilityPeriod: "PT1H"}, {Name: "b", Important: true}}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(changefeeds.Check("app/items", test.spec, test.caps), qt.IsNil)
		})
	}
}

// TestCheck_RefusesWhatTheTargetLacks pins each option to the key that gates
// it, on the newest line that lacks it.
func TestCheck_RefusesWhatTheTargetLacks(t *testing.T) {
	plain := changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON"}}
	withSIDs, withSchema, withAuto, withAvailability := plain, plain, plain, plain
	withSIDs.UserSIDs = true
	withSchema.SchemaChanges = true
	withAuto.TopicAutoPartitioning = true
	withAvailability.Consumers = []ydbdecl.Consumer{{Name: "c", AvailabilityPeriod: "PT1H"}}
	tests := []struct {
		name    string
		spec    changefeeds.Feed
		caps    map[string]bool
		want    changefeeds.Refusal
		wantErr string
	}{
		{name: "any changefeed on a target without them", spec: plain, caps: map[string]bool{},
			want:    changefeeds.Refusal{Subject: `changefeed "f" of table "app/items"`, Key: changefeeds.CapabilityChangefeeds},
			wantErr: `changefeed "f" of table "app/items", which requires target capability changefeeds, unavailable on this ydb target`},
		{name: "USER_SIDS on 25.4", spec: withSIDs, caps: line(25, 4),
			want:    changefeeds.Refusal{Subject: `changefeed "f" of table "app/items" takes USER_SIDS`, Key: changefeeds.CapabilityUserSIDs},
			wantErr: `changefeed "f" of table "app/items" takes USER_SIDS, which requires target capability changefeed_user_sids, unavailable on this ydb target`},
		{name: "SCHEMA_CHANGES on 25.2", spec: withSchema, caps: line(25, 2),
			want:    changefeeds.Refusal{Subject: `changefeed "f" of table "app/items" takes SCHEMA_CHANGES`, Key: changefeeds.CapabilitySchemaChanges},
			wantErr: `changefeed "f" of table "app/items" takes SCHEMA_CHANGES, which requires target capability changefeed_schema_changes, unavailable on this ydb target`},
		{name: "an auto-partitioned topic on 25.1", spec: withAuto, caps: line(25, 1),
			want:    changefeeds.Refusal{Subject: `changefeed "f" of table "app/items" takes TOPIC_AUTO_PARTITIONING`, Key: changefeeds.CapabilityTopicAutoPartitioning},
			wantErr: `changefeed "f" of table "app/items" takes TOPIC_AUTO_PARTITIONING, which requires target capability changefeed_topic_auto_partitioning, unavailable on this ydb target`},
		{name: "an availability period on 25.3", spec: withAvailability, caps: line(25, 3),
			want: changefeeds.Refusal{Subject: `consumer "c" of changefeed "f" of table "app/items" takes availability_period`, Key: "topic_consumer_availability_period"},
			wantErr: `consumer "c" of changefeed "f" of table "app/items" takes availability_period, which requires target capability ` +
				`topic_consumer_availability_period, unavailable on this ydb target`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got := changefeeds.Check("app/items", test.spec, test.caps)
			c.Assert(got, qt.IsNotNil)
			c.Assert(*got, qt.DeepEquals, test.want)
			c.Assert(got.Message(), qt.Equals, test.wantErr)
		})
	}
}

// TestCheck_RefusesWhatYDBRefusesEverywhere pins the shapes YDB refuses on
// every line, with no key to name.
func TestCheck_RefusesWhatYDBRefusesEverywhere(t *testing.T) {
	feed := func(spec ydbdecl.Changefeed) changefeeds.Feed { return changefeeds.Feed{Changefeed: spec} }
	disabled := feed(ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON"})
	disabled.Disabled = true
	tests := []struct {
		name       string
		spec       changefeeds.Feed
		wantReason string
	}{
		{name: "Debezium in UPDATES mode", spec: feed(ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "DEBEZIUM_JSON"}),
			wantReason: "YDB writes DEBEZIUM_JSON in every mode but UPDATES .*"},
		{name: "Debezium with virtual timestamps", spec: feed(ydbdecl.Changefeed{Name: "f", Mode: "NEW_IMAGE", Format: "DEBEZIUM_JSON", VirtualTimestamps: true}),
			wantReason: "YDB writes DEBEZIUM_JSON with no virtual timestamps, .*"},
		{name: "Debezium with resolved timestamps", spec: feed(ydbdecl.Changefeed{Name: "f", Mode: "NEW_IMAGE", Format: "DEBEZIUM_JSON", ResolvedTimestamps: "PT1S"}),
			wantReason: "YDB writes DEBEZIUM_JSON with no virtual timestamps, .*"},
		{name: "Debezium with schema changes", spec: feed(ydbdecl.Changefeed{Name: "f", Mode: "NEW_IMAGE", Format: "DEBEZIUM_JSON", SchemaChanges: true}),
			wantReason: "YDB writes DEBEZIUM_JSON with no virtual timestamps, .*"},
		{name: "a disabled changefeed", spec: disabled, wantReason: "YDB has no statement that disables a changefeed .*"},
		{name: "an unknown mode", spec: feed(ydbdecl.Changefeed{Name: "f", Mode: "ALL", Format: "JSON"}), wantReason: `its mode "ALL" is none of .*`},
		{name: "a slash in the name", spec: feed(ydbdecl.Changefeed{Name: "a/b", Mode: "UPDATES", Format: "JSON"}),
			wantReason: "a changefeed needs a name without a slash .*"},
		{name: "a fractional retention", spec: feed(ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON", RetentionPeriod: "PT1.5S"}),
			wantReason: `its retention_period "PT1.5S": interval "PT1.5S" has a fraction of a second, .*`},
		{name: "two consumers of one name", spec: feed(ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON",
			Consumers: []ydbdecl.Consumer{{Name: "c"}, {Name: "c"}}}), wantReason: `two of its consumers are named "c", .*`},
		{name: "an important consumer with an availability period", spec: feed(ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON",
			Consumers: []ydbdecl.Consumer{{Name: "c", Important: true, AvailabilityPeriod: "PT1H"}}}), wantReason: `invalid availability_period "PT1H": .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got := changefeeds.Check("t", test.spec, line(26, 2))
			c.Assert(got, qt.IsNotNil)
			c.Assert(got.Key, qt.Equals, "")
			c.Assert(got.Reason, qt.Matches, test.wantReason)
		})
	}
}

func TestNameRefusal_HappyPath(t *testing.T) {
	feed := changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "feed"}}
	tests := []struct {
		name    string
		feeds   []changefeeds.Feed
		indexes []string
		want    string
	}{
		{name: "distinct names", feeds: []changefeeds.Feed{feed}, indexes: []string{"ix"}, want: ""},
		{name: "an index's name", feeds: []changefeeds.Feed{feed}, indexes: []string{"feed"},
			want: `changefeed "feed" has the name of one of its indexes, and YDB keeps both under the table's path`},
		{name: "two changefeeds", feeds: []changefeeds.Feed{feed, feed}, want: `two of its changefeeds are named "feed"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(changefeeds.NameRefusal(test.feeds, test.indexes), qt.Equals, test.want)
		})
	}
}

func TestKeyRefusal_HappyPath(t *testing.T) {
	two := changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "f", TopicMinActivePartitions: 2}}
	tests := []struct {
		name    string
		spec    changefeeds.Feed
		keyType string
		want    string
	}{
		{name: "two partitions on a Uint64 key", spec: two, keyType: "Uint64", want: ""},
		{name: "two partitions on a Uint32 key", spec: two, keyType: "Uint32", want: ""},
		{name: "one partition on a Utf8 key", spec: changefeeds.Feed{Changefeed: ydbdecl.Changefeed{TopicMinActivePartitions: 1}}, keyType: "Utf8", want: ""},
		{name: "two partitions on a key of unknown type", spec: two, keyType: "", want: ""},
		{name: "two partitions on a Utf8 key", spec: two, keyType: "Utf8",
			want: "its topic starts with 2 partitions, which YDB splits by the first key column, and takes that " +
				"column only as Uint32 or Uint64, not Utf8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(changefeeds.KeyRefusal(test.spec, test.keyType), qt.Equals, test.want)
		})
	}
}
