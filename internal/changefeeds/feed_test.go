package changefeeds_test

import (
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// observed is a changefeed as YDB reports one YQL created with the two options
// it requires: enabled, with its topic's 24 hours and one partition.
func observed() *native.ObservedChangefeed {
	return &native.ObservedChangefeed{
		Description: &native.ChangefeedDescription{Name: "feed", Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES,
			Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, State: native.ChangefeedDescription_STATE_ENABLED},
		Topic: &native.DescribeTopicResult{RetentionPeriod: durationpb.New(24 * time.Hour),
			PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: 1}},
	}
}

func TestFromObserved_HappyPath(t *testing.T) {
	every := observed()
	every.Description.Mode, every.Description.Format = Ydb_Table.ChangefeedMode_MODE_NEW_AND_OLD_IMAGES, Ydb_Table.ChangefeedFormat_FORMAT_DEBEZIUM_JSON
	every.Description.VirtualTimestamps, every.Description.SchemaChanges, every.Description.UserSids = true, true, true
	every.Description.ResolvedTimestampsInterval = durationpb.New(90 * time.Second)
	every.Description.State = native.ChangefeedDescription_STATE_INITIAL_SCAN
	every.InitialScan = true
	every.Topic.RetentionPeriod = durationpb.New(12 * time.Hour)
	every.Topic.PartitioningSettings = &Ydb_Topic.PartitioningSettings{MinActivePartitions: 3,
		AutoPartitioningSettings: &Ydb_Topic.AutoPartitioningSettings{Strategy: Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_SCALE_UP}}
	every.Topic.Consumers = []*native.Consumer{
		{Name: "search", Attributes: map[string]string{"_service_type": "data-streams"}},
		{Name: "billing", Important: true, ReadFrom: timestamppb.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
			SupportedCodecs: &Ydb_Topic.SupportedCodecs{Codecs: []int32{int32(Ydb_Topic.Codec_CODEC_RAW), int32(Ydb_Topic.Codec_CODEC_GZIP)}}},
	}
	disabled := observed()
	disabled.Description.State = native.ChangefeedDescription_STATE_DISABLED
	replicated := observed()
	replicated.Description.Attributes = map[string]string{"__async_replication": `{"path":"/local/repl/orders","id":"1","supports_topic_autopartitioning":false}`}
	tests := []struct {
		name     string
		observed *native.ObservedChangefeed
		want     changefeeds.Feed
	}{
		{name: "the two options YQL requires, with YDB's defaults left out", observed: observed(),
			want: changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "feed", Mode: "UPDATES", Format: "JSON"}}},
		{name: "every option, consumers by name", observed: every,
			want: changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "feed", Mode: "NEW_AND_OLD_IMAGES", Format: "DEBEZIUM_JSON", VirtualTimestamps: true,
				ResolvedTimestamps: "PT1M30S", InitialScan: true, UserSIDs: true, SchemaChanges: true, TopicMinActivePartitions: 3, TopicAutoPartitioning: true,
				RetentionPeriod: "PT12H", Consumers: []ydbdecl.Consumer{
					{Name: "billing", Important: true, ReadFrom: "2026-01-01T00:00:00Z", SupportedCodecs: []string{"raw", "gzip"}},
					{Name: "search"},
				}}}},
		{name: "a disabled changefeed", observed: disabled,
			want: changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "feed", Mode: "UPDATES", Format: "JSON"}, Disabled: true}},
		{name: "a stream a replication controller owns", observed: replicated,
			want: changefeeds.Feed{Changefeed: ydbdecl.Changefeed{Name: "feed", Mode: "UPDATES", Format: "JSON"}, Replicated: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := changefeeds.FromObserved(test.observed)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
		})
	}
}

// TestFromObserved_FailurePath holds the settings the host's reader records as
// not described rather than reads.
func TestFromObserved_FailurePath(t *testing.T) {
	edit := func(change func(*native.ObservedChangefeed)) *native.ObservedChangefeed {
		feed := observed()
		change(feed)
		return feed
	}
	tests := []struct {
		name     string
		observed *native.ObservedChangefeed
		wantErr  string
	}{
		{name: "a format Ptah does not write", observed: edit(func(f *native.ObservedChangefeed) {
			f.Description.Format = Ydb_Table.ChangefeedFormat_FORMAT_DYNAMODB_STREAMS_JSON
		}), wantErr: `its format FORMAT_DYNAMODB_STREAMS_JSON is not one Ptah writes`},
		{name: "an AWS region", observed: edit(func(f *native.ObservedChangefeed) { f.Description.AwsRegion = "eu-west-1" }),
			wantErr: `it names an AWS region, which Ptah does not model`},
		{name: "an attribute", observed: edit(func(f *native.ObservedChangefeed) { f.Description.Attributes = map[string]string{"owner": "team"} }),
			wantErr: `it holds attributes, which Ptah does not model`},
		{name: "a replication binding beside another attribute", observed: edit(func(f *native.ObservedChangefeed) {
			f.Description.Attributes = map[string]string{"__async_replication": `{"path":"/p","id":"1","supports_topic_autopartitioning":true}`, "owner": "team"}
		}), wantErr: `it holds attributes, which Ptah does not model`},
		{name: "a replication binding without its fields", observed: edit(func(f *native.ObservedChangefeed) {
			f.Description.Attributes = map[string]string{"__async_replication": `{"path":"/p"}`}
		}), wantErr: `its replication binding is not one Ptah reads`},
		{name: "a state Ptah does not read", observed: edit(func(f *native.ObservedChangefeed) {
			f.Description.State = native.ChangefeedDescription_STATE_UNSPECIFIED
		}),
			wantErr: `its state STATE_UNSPECIFIED is not one Ptah reads`},
		{name: "a fraction of a second", observed: edit(func(f *native.ObservedChangefeed) {
			f.Description.ResolvedTimestampsInterval = durationpb.New(1500 * time.Millisecond)
		}), wantErr: `its resolved timestamps interval is 1.5s, which is not a whole number of seconds a declaration can hold`},
		{name: "a strategy no changefeed declares", observed: edit(func(f *native.ObservedChangefeed) {
			f.Topic.PartitioningSettings.AutoPartitioningSettings = &Ydb_Topic.AutoPartitioningSettings{Strategy: Ydb_Topic.AutoPartitioningStrategy_AUTO_PARTITIONING_STRATEGY_SCALE_UP_AND_DOWN}
		}), wantErr: `its topic's auto-partitioning strategy AUTO_PARTITIONING_STRATEGY_SCALE_UP_AND_DOWN is not one a changefeed declares`},
		{name: "a consumer attribute", observed: edit(func(f *native.ObservedChangefeed) {
			f.Topic.Consumers = []*native.Consumer{{Name: "c", Attributes: map[string]string{"owner": "team"}}}
		}), wantErr: `consumer "c" holds attributes, which Ptah does not model`},
		{name: "a consumer codec Ptah does not declare", observed: edit(func(f *native.ObservedChangefeed) {
			f.Topic.Consumers = []*native.Consumer{{Name: "c", SupportedCodecs: &Ydb_Topic.SupportedCodecs{Codecs: []int32{10042}}}}
		}), wantErr: `consumer "c": its codec list names codec 10042, which Ptah does not declare`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := changefeeds.FromObserved(test.observed)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.DeepEquals, changefeeds.Feed{})
		})
	}
}

// TestIntent_RoundTrip holds that a declaration written as the desired model
// reads back as the declaration, in the case YDB keeps.
func TestIntent_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		spec ydbdecl.Changefeed
		want ydbdecl.Changefeed
	}{
		{name: "the two options YDB requires", spec: ydbdecl.Changefeed{Name: "f", Mode: "updates", Format: "json"},
			want: ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON"}},
		{name: "every option", spec: ydbdecl.Changefeed{Name: "f", Mode: "NEW_IMAGE", Format: "JSON", VirtualTimestamps: true, ResolvedTimestamps: "PT90S",
			InitialScan: true, UserSIDs: true, SchemaChanges: true, TopicMinActivePartitions: 2, TopicAutoPartitioning: true, RetentionPeriod: "pt12h",
			Consumers: []ydbdecl.Consumer{{Name: "b", Important: true, ReadFrom: "2026-01-01T00:00:00Z", SupportedCodecs: []string{"raw"}}, {Name: "a", AvailabilityPeriod: "PT1H"}}},
			want: ydbdecl.Changefeed{Name: "f", Mode: "NEW_IMAGE", Format: "JSON", VirtualTimestamps: true, ResolvedTimestamps: "PT1M30S",
				InitialScan: true, UserSIDs: true, SchemaChanges: true, TopicMinActivePartitions: 2, TopicAutoPartitioning: true, RetentionPeriod: "PT12H",
				Consumers: []ydbdecl.Consumer{{Name: "b", Important: true, ReadFrom: "2026-01-01T00:00:00Z", SupportedCodecs: []string{"raw"}}, {Name: "a", AvailabilityPeriod: "PT1H"}}}},
		{name: "YDB's 24 hours and one partition read as left out", spec: ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON", RetentionPeriod: "P1D", TopicMinActivePartitions: 1},
			want: ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			intent, err := changefeeds.Intent(test.spec)
			c.Assert(err, qt.IsNil)
			got, err := changefeeds.FromIntent(intent)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, changefeeds.Feed{Changefeed: test.want})
		})
	}
}

func TestIntent_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		spec    ydbdecl.Changefeed
		wantErr string
	}{
		{name: "an unknown mode", spec: ydbdecl.Changefeed{Name: "f", Mode: "ALL", Format: "JSON"},
			wantErr: `changefeed "f": its mode "ALL" is none of KEYS_ONLY, UPDATES, NEW_IMAGE, OLD_IMAGE, NEW_AND_OLD_IMAGES`},
		{name: "an unknown format", spec: ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "XML"},
			wantErr: `changefeed "f": its format "XML" is none of JSON, DEBEZIUM_JSON`},
		{name: "a zero interval", spec: ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON", ResolvedTimestamps: "PT0S"},
			wantErr: `changefeed "f": resolved_timestamps: interval "PT0S" is no time at all, and YDB takes only a positive interval`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := changefeeds.Intent(test.spec)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.IsNil)
		})
	}
}

// TestFromIntent_FailurePath holds what a declaration cannot state.
func TestFromIntent_FailurePath(t *testing.T) {
	plain := func() *native.ChangefeedIntent {
		return &native.ChangefeedIntent{Name: "f", Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES, Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, Topic: &native.TopicIntent{}}
	}
	region, attributes, consumer, mode := plain(), plain(), plain(), plain()
	region.AwsRegion = "eu-west-1"
	attributes.Attributes = map[string]string{"owner": "team"}
	consumer.Topic.Consumers = []*native.ConsumerIntent{{Name: "c", Attributes: map[string]string{"owner": "team"}}}
	mode.Mode = Ydb_Table.ChangefeedMode_MODE_UNSPECIFIED
	tests := []struct {
		name    string
		intent  *native.ChangefeedIntent
		wantErr string
	}{
		{name: "no declaration", wantErr: `missing YDB changefeed declaration`},
		{name: "an AWS region", intent: region, wantErr: `changefeed "f": the declaration names an AWS region, which no YQL statement sets`},
		{name: "an attribute", intent: attributes, wantErr: `changefeed "f": it holds attributes, which Ptah does not model`},
		{name: "a consumer attribute", intent: consumer, wantErr: `changefeed "f": consumer "c": the declaration states attributes, which no YQL statement sets`},
		{name: "no mode", intent: mode, wantErr: `changefeed "f": its mode MODE_UNSPECIFIED is none of .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got, err := changefeeds.FromIntent(test.intent)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.DeepEquals, changefeeds.Feed{})
		})
	}
}
