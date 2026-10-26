package ydbdecl_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// The cases are the host's internal/ydbchangefeed parse cases: a YAML, a Go
// and a YQL declaration of a changefeed all read through ParseChangefeed.

func TestParseChangefeed_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		want   ydbdecl.Changefeed
	}{
		{
			name:   "the two options YDB requires, folded to capitals",
			values: map[string]string{"name": "updates", "mode": "updates", "format": "json"},
			want:   ydbdecl.Changefeed{Name: "updates", Mode: "UPDATES", Format: "JSON"},
		},
		{
			name: "every option",
			values: map[string]string{
				"name": "feed", "mode": "NEW_AND_OLD_IMAGES", "format": "DEBEZIUM_JSON",
				"virtual_timestamps": "true", "resolved_timestamps": "PT10S", "initial_scan": "TRUE",
				"user_sids": "true", "schema_changes": "true", "topic_min_active_partitions": "4",
				"topic_auto_partitioning": "true", "retention_period": "P1D",
			},
			want: ydbdecl.Changefeed{
				Name: "feed", Mode: "NEW_AND_OLD_IMAGES", Format: "DEBEZIUM_JSON", VirtualTimestamps: true,
				ResolvedTimestamps: "PT10S", InitialScan: true, UserSIDs: true, SchemaChanges: true,
				TopicMinActivePartitions: 4, TopicAutoPartitioning: true, RetentionPeriod: "P1D",
			},
		},
		{
			name:   "a switch set to false declares nothing",
			values: map[string]string{"name": "f", "mode": "KEYS_ONLY", "format": "JSON", "initial_scan": "false"},
			want:   ydbdecl.Changefeed{Name: "f", Mode: "KEYS_ONLY", Format: "JSON"},
		},
		{
			name:   "a key the changefeed does not name is ignored",
			values: map[string]string{"name": "f", "mode": "UPDATES", "format": "JSON", "table": "items"},
			want:   ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := ydbdecl.ParseChangefeed(test.values)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
		})
	}
}

func TestParseChangefeed_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]string
		wantErr string
	}{
		{name: "no name", values: map[string]string{"mode": "UPDATES", "format": "JSON"},
			wantErr: `invalid name "": a changefeed needs a name`},
		{name: "no mode", values: map[string]string{"name": "f", "format": "JSON"},
			wantErr: `invalid mode "": takes one of KEYS_ONLY, UPDATES, NEW_IMAGE, OLD_IMAGE, NEW_AND_OLD_IMAGES`},
		{name: "no format", values: map[string]string{"name": "f", "mode": "UPDATES"},
			wantErr: `invalid format "": takes one of JSON, DEBEZIUM_JSON`},
		{name: "the document-table format", values: map[string]string{"name": "f", "mode": "UPDATES", "format": "dynamodb_streams_json"},
			wantErr: `invalid format "dynamodb_streams_json": YDB writes .*`},
		{name: "a switch that is not a boolean", values: map[string]string{"name": "f", "mode": "UPDATES", "format": "JSON", "virtual_timestamps": "yes"},
			wantErr: `invalid virtual_timestamps "yes": takes true or false`},
		{name: "a fraction of a second", values: map[string]string{"name": "f", "mode": "UPDATES", "format": "JSON", "retention_period": "PT1.5S"},
			wantErr: `invalid retention_period "PT1.5S": interval "PT1.5S" has a fraction of a second, .*`},
		{name: "a zero interval", values: map[string]string{"name": "f", "mode": "UPDATES", "format": "JSON", "resolved_timestamps": "PT0S"},
			wantErr: `invalid resolved_timestamps "PT0S": interval "PT0S" is no time at all, and YDB takes only a positive interval`},
		{name: "zero partitions", values: map[string]string{"name": "f", "mode": "UPDATES", "format": "JSON", "topic_min_active_partitions": "0"},
			wantErr: `invalid topic_min_active_partitions "0": takes a count of at least 1 .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := ydbdecl.ParseChangefeed(test.values)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.DeepEquals, ydbdecl.Changefeed{})
		})
	}
}

// TestFeedSeconds_HappyPath reads an interval in YDB's grammar, in any case.
func TestFeedSeconds_HappyPath(t *testing.T) {
	tests := []struct {
		text string
		want uint64
	}{
		{text: "PT1S", want: 1},
		{text: "PT90M", want: 5400},
		{text: "P1DT2H30M5S", want: 95405},
		{text: "P2W3D", want: 17 * 86400},
		{text: "pt12h", want: 43200},
	}
	for _, test := range tests {
		t.Run(test.text, func(t *testing.T) {
			c := qt.New(t)
			got, err := ydbdecl.FeedSeconds(test.text)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

// TestFeedSeconds_FailurePath refuses an interval of no length, which a TTL
// takes and a changefeed does not, and what YDB's grammar refuses.
func TestFeedSeconds_FailurePath(t *testing.T) {
	tests := []struct {
		text    string
		wantErr string
	}{
		{text: "PT0S", wantErr: `interval "PT0S" is no time at all, and YDB takes only a positive interval`},
		{text: "PT0.5S", wantErr: `interval "PT0.5S" has a fraction of a second, .*`},
		{text: "P1M", wantErr: `interval "P1M" is not an ISO 8601 duration YDB takes .*`},
	}
	for _, test := range tests {
		t.Run(test.text, func(t *testing.T) {
			c := qt.New(t)
			got, err := ydbdecl.FeedSeconds(test.text)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.Equals, uint64(0))
		})
	}
}

func TestFormatInterval_HappyPath(t *testing.T) {
	for _, test := range []struct {
		seconds uint64
		want    string
	}{{0, "PT0S"}, {1, "PT1S"}, {90, "PT1M30S"}, {43200, "PT12H"}, {86400, "P1D"}, {129600, "P1DT12H"}} {
		t.Run(test.want, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(ydbdecl.FormatInterval(test.seconds), qt.Equals, test.want)
		})
	}
}
