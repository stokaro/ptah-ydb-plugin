package nativeapi_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Topic_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
)

// feedServer is a table at /local/app/records and the topics of its
// changefeeds. Its statement runner applies the statements a changefeed
// operation sends, the way YDB answers them, unless told to ignore them.
type feedServer struct {
	feeds      []*native.ChangefeedDescription
	topics     map[string]*native.DescribeTopicResult
	statements []string
	ignore     bool
	fail       bool
}

func (s *feedServer) Describe(_ context.Context, table string) (*native.DescribeTableResult, error) {
	if table != "/local/app/records" {
		return nil, errors.New("no such table")
	}
	described := &native.DescribeTableResult{Self: &Ydb_Scheme.Entry{Name: "records", Type: Ydb_Scheme.Entry_TABLE}}
	for _, feed := range s.feeds {
		described.Changefeeds = append(described.Changefeeds, proto.CloneOf(feed))
	}
	return described, nil
}

func (*feedServer) Close() error { return nil }

func (s *feedServer) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("unexpected stream")
}

func (s *feedServer) Invoke(_ context.Context, method string, args, reply any, _ ...grpc.CallOption) error {
	if !strings.HasSuffix(method, "/DescribeTopic") {
		return errors.New("unexpected call " + method)
	}
	topic, found := s.topics[args.(*Ydb_Topic.DescribeTopicRequest).Path]
	if !found {
		return errors.New("no such topic")
	}
	encoded, err := proto.Marshal(topic)
	if err != nil {
		return err
	}
	// The server answers with its own message, which the provider reads into
	// its definition of the same wire format.
	reply.(*Ydb_Topic.DescribeTopicResponse).Operation = &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS,
		Result: &anypb.Any{TypeUrl: "type.googleapis.com/Ydb.Topic.DescribeTopicResult", Value: encoded}}
	return nil
}

// quoted returns the first backquoted name after marker in statement.
func quoted(statement, marker string) string {
	rest := strings.SplitN(statement, marker+" `", 2)[1]
	return rest[:strings.Index(rest, "`")]
}

// execute applies one statement: an added changefeed of the mode it names,
// with its topic's defaults, a dropped one, an added consumer and a longer
// retention.
func (s *feedServer) execute(_ context.Context, statement string) error {
	s.statements = append(s.statements, statement)
	if s.fail {
		return errors.New("statement failed")
	}
	if s.ignore {
		return nil
	}
	switch {
	case strings.Contains(statement, "ADD CHANGEFEED"):
		name := quoted(statement, "ADD CHANGEFEED")
		mode := Ydb_Table.ChangefeedMode_MODE_UPDATES
		if strings.Contains(statement, "MODE = 'KEYS_ONLY'") {
			mode = Ydb_Table.ChangefeedMode_MODE_KEYS_ONLY
		}
		s.feeds = append(s.feeds, &native.ChangefeedDescription{Name: name, Mode: mode,
			Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, State: native.ChangefeedDescription_STATE_ENABLED})
		s.topics["/local/app/records/"+name] = &native.DescribeTopicResult{Self: &Ydb_Scheme.Entry{Name: name, Type: Ydb_Scheme.Entry_TOPIC},
			PartitioningSettings: &Ydb_Topic.PartitioningSettings{MinActivePartitions: 1}, RetentionPeriod: durationpb.New(24 * time.Hour)}
	case strings.Contains(statement, "DROP CHANGEFEED"):
		name := quoted(statement, "DROP CHANGEFEED")
		s.feeds = slices.DeleteFunc(s.feeds, func(feed *native.ChangefeedDescription) bool { return feed.Name == name })
		delete(s.topics, "/local/app/records/"+name)
	case strings.Contains(statement, "ADD CONSUMER"):
		topic := s.topics[quoted(statement, "ALTER TOPIC")]
		topic.Consumers = append(topic.Consumers, &native.Consumer{Name: quoted(statement, "ADD CONSUMER"),
			Attributes: map[string]string{"_service_type": "data-streams"}})
	case strings.Contains(statement, "SET (retention_period = Interval('P2D'))"):
		s.topics[quoted(statement, "ALTER TOPIC")].RetentionPeriod = durationpb.New(48 * time.Hour)
	default:
		return errors.New("unexpected statement")
	}
	return nil
}

// withFeed is a server whose table holds changefeed f in UPDATES mode.
func withFeed() *feedServer {
	server := &feedServer{topics: map[string]*native.DescribeTopicResult{}}
	_ = server.execute(context.Background(), "ALTER TABLE `/local/app/records` ADD CHANGEFEED `f` WITH (MODE = 'UPDATES', FORMAT = 'JSON');")
	server.statements = nil
	return server
}

func changefeedApplier(server *feedServer) *nativeapi.Changefeed {
	return &nativeapi.Changefeed{Open: func(context.Context) (nativeapi.ChangefeedTable, error) { return server, nil },
		Topics: Ydb_Topic_V1.NewTopicServiceClient(server), Execute: server.execute}
}

// operation captures one changefeed step of app.records.
func feedOperation(c *qt.C, changes ...changefeeds.Change) *protocol.Envelope {
	c.Helper()
	captured, err := schema.CaptureChangefeedOperation("op", &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}, changes)
	c.Assert(err, qt.IsNil)
	return captured.Value
}

// read is the changefeed f a server holds, projected as inspection captures it.
func readFeed(c *qt.C, server *feedServer) *native.ObservedChangefeed {
	c.Helper()
	feed, err := schema.ProjectChangefeed(&native.ObservedChangefeed{Description: server.feeds[0], Topic: server.topics["/local/app/records/f"]})
	c.Assert(err, qt.IsNil)
	return feed
}

func declaredFeed(c *qt.C, spec ydbdecl.Changefeed) *native.ChangefeedIntent {
	c.Helper()
	intent, err := changefeeds.Intent(spec)
	c.Assert(err, qt.IsNil)
	return intent
}

func allowed(context.Context) error { return nil }

func TestChangefeedApply_HappyPath(t *testing.T) {
	updates := ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON", Consumers: []ydbdecl.Consumer{{Name: "c"}}}
	longer := ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON", RetentionPeriod: "P2D"}
	for _, test := range []struct {
		name       string
		server     *feedServer
		changes    func(*qt.C, *feedServer) []changefeeds.Change
		statements []string
		// held are the changefeeds the table holds afterward.
		held []string
	}{
		{"an addition with a consumer", &feedServer{topics: map[string]*native.DescribeTopicResult{}},
			func(c *qt.C, _ *feedServer) []changefeeds.Change {
				return []changefeeds.Change{{Name: "f", After: declaredFeed(c, updates)}}
			},
			[]string{"ALTER TABLE `/local/app/records` ADD CHANGEFEED `f` WITH (MODE = 'UPDATES', FORMAT = 'JSON');",
				"ALTER TOPIC `/local/app/records/f` ADD CONSUMER `c`;"}, []string{"f"}},
		{"a removal", withFeed(),
			func(c *qt.C, server *feedServer) []changefeeds.Change {
				return []changefeeds.Change{{Name: "f", Before: readFeed(c, server)}}
			},
			[]string{"ALTER TABLE `/local/app/records` DROP CHANGEFEED `f`;"}, nil},
		{"a recreation beside an addition, the removal first", withFeed(),
			func(c *qt.C, server *feedServer) []changefeeds.Change {
				return []changefeeds.Change{{Name: "a", After: declaredFeed(c, ydbdecl.Changefeed{Name: "a", Mode: "UPDATES", Format: "JSON"})},
					{Name: "f", Before: readFeed(c, server), After: declaredFeed(c, ydbdecl.Changefeed{Name: "f", Mode: "KEYS_ONLY", Format: "JSON"})}}
			},
			[]string{"ALTER TABLE `/local/app/records` DROP CHANGEFEED `f`;",
				"ALTER TABLE `/local/app/records` ADD CHANGEFEED `a` WITH (MODE = 'UPDATES', FORMAT = 'JSON');",
				"ALTER TABLE `/local/app/records` ADD CHANGEFEED `f` WITH (MODE = 'KEYS_ONLY', FORMAT = 'JSON');"}, []string{"a", "f"}},
		{"a longer retention", withFeed(),
			func(c *qt.C, server *feedServer) []changefeeds.Change {
				return []changefeeds.Change{{Name: "f", Before: readFeed(c, server), After: declaredFeed(c, longer)}}
			},
			[]string{"ALTER TOPIC `/local/app/records/f` SET (retention_period = Interval('P2D'));"}, []string{"f"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			dispatched, err := changefeedApplier(test.server).Apply(c.Context(), "/local", feedOperation(c, test.changes(c, test.server)...), allowed)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(test.server.statements, qt.DeepEquals, test.statements)
			var held []string
			for _, feed := range test.server.feeds {
				held = append(held, feed.Name)
			}
			slices.Sort(held)
			c.Assert(held, qt.DeepEquals, test.held)
		})
	}
}

func TestChangefeedApply_FailurePath(t *testing.T) {
	updates := ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON"}
	keysOnly := func(c *qt.C) *native.ObservedChangefeed {
		c.Helper()
		other := withFeed()
		other.feeds[0].Mode = Ydb_Table.ChangefeedMode_MODE_KEYS_ONLY
		return readFeed(c, other)
	}
	ignoring, failing := &feedServer{topics: map[string]*native.DescribeTopicResult{}, ignore: true}, &feedServer{topics: map[string]*native.DescribeTopicResult{}, fail: true}
	for _, test := range []struct {
		name       string
		server     *feedServer
		database   string
		step       func(*qt.C) changefeeds.Change
		before     func(context.Context) error
		dispatched bool
		wantErr    string
	}{
		{"an addition of a changefeed the table holds", withFeed(), "/local",
			func(c *qt.C) changefeeds.Change {
				return changefeeds.Change{Name: "f", After: declaredFeed(c, updates)}
			}, allowed, false,
			`changefeed "f" of table "app/records" exists, and the plan read none`},
		{"a removal of a changefeed another writer changed", withFeed(), "/local",
			func(c *qt.C) changefeeds.Change { return changefeeds.Change{Name: "f", Before: keysOnly(c)} }, allowed, false,
			`changefeed "f" of table "app/records" changed since the plan read it`},
		{"a removal of a changefeed that is gone", &feedServer{topics: map[string]*native.DescribeTopicResult{}}, "/local",
			func(c *qt.C) changefeeds.Change { return changefeeds.Change{Name: "f", Before: keysOnly(c)} }, allowed, false,
			`changefeed "f" of table "app/records" is gone since the plan read it`},
		{"a lock lost before the statement", &feedServer{topics: map[string]*native.DescribeTopicResult{}}, "/local",
			func(c *qt.C) changefeeds.Change {
				return changefeeds.Change{Name: "f", After: declaredFeed(c, updates)}
			},
			func(context.Context) error { return errors.New("lock lost") }, false, `lock lost`},
		{"a statement the server refuses", failing, "/local",
			func(c *qt.C) changefeeds.Change {
				return changefeeds.Change{Name: "f", After: declaredFeed(c, updates)}
			}, allowed, true, `statement failed`},
		{"an addition the server did not make", ignoring, "/local",
			func(c *qt.C) changefeeds.Change {
				return changefeeds.Change{Name: "f", After: declaredFeed(c, updates)}
			}, allowed, true,
			`changefeed "f" of table "app/records" is absent after the operation`},
		{"a table of another database", &feedServer{topics: map[string]*native.DescribeTopicResult{}}, "/other",
			func(c *qt.C) changefeeds.Change {
				return changefeeds.Change{Name: "f", After: declaredFeed(c, updates)}
			}, allowed, false, `no such table`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			dispatched, err := changefeedApplier(test.server).Apply(c.Context(), test.database, feedOperation(c, test.step(c)), test.before)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(dispatched, qt.Equals, test.dispatched)
			c.Assert(len(test.server.statements) > 0, qt.Equals, test.dispatched)
		})
	}
}

// TestChangefeedApply_RefusesAnOwnerOfAnotherDatabase refuses an operation
// whose owner names a database other than the session's before it reads.
func TestChangefeedApply_RefusesAnOwnerOfAnotherDatabase(t *testing.T) {
	c := qt.New(t)
	server := &feedServer{topics: map[string]*native.DescribeTopicResult{}}
	captured, err := schema.CaptureChangefeedOperation("op", &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/other", Schema: "app", Name: "records"},
		[]changefeeds.Change{{Name: "f", After: declaredFeed(c, ydbdecl.Changefeed{Name: "f", Mode: "UPDATES", Format: "JSON"})}})
	c.Assert(err, qt.IsNil)
	dispatched, err := changefeedApplier(server).Apply(c.Context(), "/local", captured.Value, allowed)
	c.Assert(err, qt.ErrorMatches, `changefeed operation targets another database`)
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(server.statements, qt.HasLen, 0)
}
