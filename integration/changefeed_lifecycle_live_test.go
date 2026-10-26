//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
	"github.com/ydb-platform/ydb-go-sdk/v3/topic/topictypes"
	"google.golang.org/protobuf/encoding/protojson"
)

// TestChangefeedLifecycleSubprocess drives a changefeed through the provider's
// operations the way a plan carries them out, and reads the table and the
// changefeed's topic back from the server after each step: an addition with an
// initial scan and two consumers, a consumer YDB changes only by dropping it
// and adding it again, a change another writer made after the plan read the
// changefeed, a mode change that drops the changefeed and adds it again in one
// operation, and a removal.
func TestChangefeedLifecycleSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	absolute := path.Join(database, directory, "events")
	c.Assert(execFixtureSchema(ctx, opened.DB, "CREATE TABLE `"+absolute+"` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id))"), qt.IsNil)
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+absolute+"`"), qt.IsNil)
	})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Capabilities[changefeeds.CapabilityChangefeeds], qt.IsTrue)
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: directory, Name: "events"}

	// held reads the changefeed as inspection captures it, nil where the
	// table holds none.
	held := func() *native.ObservedChangefeed {
		c.Helper()
		snapshot, err := inspect.Read(ctx, opened.Driver, columnTables(opened), database, []string{directory})
		c.Assert(err, qt.IsNil)
		c.Assert(snapshot.Tables, qt.HasLen, 1)
		for _, facet := range snapshot.Tables[0].Facets {
			if facet.Kind != schema.ChangefeedProperties {
				continue
			}
			var set native.ChangefeedObservations
			c.Assert(protojson.Unmarshal(facet.Payload, &set), qt.IsNil)
			for _, feed := range set.Changefeeds {
				if feed.Description.Name == "feed" {
					return feed
				}
			}
		}
		return nil
	}
	intent := func(spec ydbdecl.Changefeed) *native.ChangefeedIntent {
		c.Helper()
		declared, err := changefeeds.Intent(spec)
		c.Assert(err, qt.IsNil)
		return declared
	}
	units := 0
	execute := func(expected protocol.Execution_Outcome, changes ...changefeeds.Change) {
		c.Helper()
		units++
		operation, err := schema.CaptureChangefeedOperation("op", owner, changes)
		c.Assert(err, qt.IsNil)
		request := &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "changefeed-" + strconv.Itoa(units),
			Units: []*protocol.Unit{{ChangeId: "change-" + strconv.Itoa(units), Body: &protocol.Unit_Native{Native: operation.Value}}}}
		result, err := client.Execute(ctx, request)
		if expected == protocol.Execution_APPLIED {
			c.Assert(err, qt.IsNil)
		} else {
			c.Assert(err, qt.ErrorIs, provider.ErrExecution)
		}
		c.Assert(result.Outcome, qt.Equals, expected, qt.Commentf("%s", result.GetMessage()))
	}
	describedFeeds := func() []options.ChangefeedDescription {
		c.Helper()
		var described options.Description
		c.Assert(opened.Driver.Table().Do(ctx, func(ctx context.Context, s table.Session) error {
			var err error
			described, err = s.DescribeTable(ctx, absolute)
			return err
		}), qt.IsNil)
		return described.Changefeeds
	}
	topic := func() topictypes.TopicDescription {
		c.Helper()
		description, err := opened.Driver.Topic().Describe(ctx, path.Join(absolute, "feed"))
		c.Assert(err, qt.IsNil)
		return description
	}
	consumers := func(description topictypes.TopicDescription) map[string][]topictypes.Codec {
		result := map[string][]topictypes.Codec{}
		for _, consumer := range description.Consumers {
			// The topic service reports an empty list for a consumer that
			// takes any codec; nil stands for it here.
			result[consumer.Name] = append([]topictypes.Codec(nil), consumer.SupportedCodecs...)
		}
		return result
	}

	created := ydbdecl.Changefeed{Name: "feed", Mode: "NEW_AND_OLD_IMAGES", Format: "JSON", VirtualTimestamps: true, InitialScan: true, RetentionPeriod: "PT12H",
		Consumers: []ydbdecl.Consumer{{Name: "billing", Important: true}, {Name: "audit", SupportedCodecs: []string{"raw"}}}}
	execute(protocol.Execution_APPLIED, changefeeds.Change{Name: "feed", After: intent(created)})
	feeds := describedFeeds()
	c.Assert(feeds, qt.HasLen, 1)
	c.Assert(feeds[0].Name, qt.Equals, "feed")
	c.Assert(feeds[0].Mode, qt.Equals, options.ChangefeedModeNewAndOldImages)
	c.Assert(feeds[0].VirtualTimestamp, qt.IsTrue)
	c.Assert(topic().RetentionPeriod, qt.Equals, 12*time.Hour)
	c.Assert(consumers(topic()), qt.DeepEquals, map[string][]topictypes.Codec{"billing": nil, "audit": {topictypes.CodecRaw}})
	read, err := changefeeds.FromObserved(held())
	c.Assert(err, qt.IsNil)
	c.Assert(read.InitialScan, qt.IsTrue)
	c.Assert(changefeeds.Equal(changefeeds.Feed{Changefeed: created}, read), qt.IsTrue)

	// No statement empties a consumer's codec list, so the consumer is
	// dropped and added again.
	restarted := created
	restarted.Consumers = []ydbdecl.Consumer{{Name: "billing", Important: true}, {Name: "audit"}}
	execute(protocol.Execution_APPLIED, changefeeds.Change{Name: "feed", Before: held(), After: intent(restarted)})
	c.Assert(consumers(topic()), qt.DeepEquals, map[string][]topictypes.Codec{"billing": nil, "audit": nil})

	// A change planned against a changefeed another writer changed since is
	// refused before any statement runs.
	stale := held()
	c.Assert(execFixtureSchema(ctx, opened.DB, "ALTER TOPIC `"+path.Join(absolute, "feed")+"` SET (retention_period = Interval('P3D'))"), qt.IsNil)
	longer := restarted
	longer.RetentionPeriod = "P2D"
	execute(protocol.Execution_NOT_STARTED, changefeeds.Change{Name: "feed", Before: stale, After: intent(longer)})
	c.Assert(topic().RetentionPeriod, qt.Equals, 72*time.Hour)
	execute(protocol.Execution_APPLIED, changefeeds.Change{Name: "feed", Before: held(), After: intent(longer)})
	c.Assert(topic().RetentionPeriod, qt.Equals, 48*time.Hour)

	// YDB changes no option of a changefeed in place: a mode change drops it
	// and adds it again, and its consumers start over.
	keys := ydbdecl.Changefeed{Name: "feed", Mode: "KEYS_ONLY", Format: "JSON"}
	execute(protocol.Execution_APPLIED, changefeeds.Change{Name: "feed", Before: held(), After: intent(keys)})
	feeds = describedFeeds()
	c.Assert(feeds, qt.HasLen, 1)
	c.Assert(feeds[0].Mode, qt.Equals, options.ChangefeedModeKeysOnly)
	c.Assert(topic().Consumers, qt.HasLen, 0)
	c.Assert(topic().RetentionPeriod, qt.Equals, 24*time.Hour)

	execute(protocol.Execution_APPLIED, changefeeds.Change{Name: "feed", Before: held()})
	c.Assert(describedFeeds(), qt.HasLen, 0)
	c.Assert(held(), qt.IsNil)
}
