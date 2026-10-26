//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"
	"time"
	"unicode"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/draft/Ydb_DynamicConfig_V1"
	"github.com/ydb-platform/ydb-go-genproto/draft/protos/Ydb_DynamicConfig"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/encoding/protojson"
)

// monitoredAddress returns the test address, which names the server's
// monitoring endpoint: a column table cannot be read without it.
func monitoredAddress(c *qt.C) string {
	c.Helper()
	address := os.Getenv("PTAH_YDB_TEST_URL")
	parsed, err := connection.Parse(address)
	c.Assert(err, qt.IsNil)
	c.Assert(parsed.Monitoring(), qt.IsNotNil, qt.Commentf("PTAH_YDB_TEST_URL must name the server's monitoring endpoint (monitoring=http://host:8765)"))
	return address
}

// withoutMonitoring returns address without its monitoring parameter.
func withoutMonitoring(c *qt.C, address string) string {
	c.Helper()
	parsed, err := url.Parse(address)
	c.Assert(err, qt.IsNil)
	query := parsed.Query()
	query.Del("monitoring")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// createColumnTable creates a column table in the fixture directory with YQL
// and drops it when the test ends.
func createColumnTable(c *qt.C, opened *connection.Connection, absolute, definition string) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := opened.DB.ExecContext(ctx, "CREATE TABLE `"+absolute+"` "+definition)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE `"+absolute+"`"), qt.IsNil)
	})
}

// TestColumnTableInspectionSubprocess inspects a directory that holds a row
// table and a column table. The column table reads back with its hash key, the
// shard count it was created with, which the table service leaves out, and
// its TTL, and with no row-table facet.
func TestColumnTableInspectionSubprocess(t *testing.T) {
	c := qt.New(t)
	address := monitoredAddress(c)
	opened, directory, database := inspectionFixture(c)
	createFixtureTable(c, opened, database, directory)
	createColumnTable(c, opened, path.Join(database, directory, "events"),
		"(ts Timestamp NOT NULL, id Uint64 NOT NULL, body Utf8, PRIMARY KEY (ts, id)) PARTITION BY HASH(id) "+
			"WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3, TTL = Interval(\"P1D\") ON ts)")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}
	snapshot, err := client.Inspect(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 2)
	events, records := snapshot.Tables[0], snapshot.Tables[1]
	c.Assert(events.Identity.Name, qt.Equals, "events")
	c.Assert(records.Identity.Name, qt.Equals, "records")
	var columns []string
	for _, column := range events.Columns {
		columns = append(columns, fmt.Sprintf("%s:%s:%t:%t", column.Name, column.Observed.ColumnType, column.Observed.IsNullable == "YES", column.Observed.PrimaryKey))
		c.Assert(column.Facets, qt.HasLen, 1)
		c.Assert(column.Facets[0].Kind, qt.Equals, schema.ColumnProperties)
	}
	c.Assert(columns, qt.DeepEquals, []string{"ts:Timestamp:false:true", "id:Uint64:false:true", "body:Utf8:true:false"})
	c.Assert(events.Facets, qt.HasLen, 1)
	c.Assert(events.Facets[0].Kind, qt.Equals, schema.ColumnStorageProperties)
	var storage native.ColumnStorageObservation
	c.Assert(protojson.Unmarshal(events.Facets[0].Payload, &storage), qt.IsNil)
	c.Assert(storage.HashColumns, qt.DeepEquals, []string{"id"})
	c.Assert(storage.Partitions, qt.Equals, uint64(3))
	c.Assert(storage.DataChannelCount, qt.Equals, uint64(64))
	c.Assert(storage.GetTtl().GetEnabled().GetColumn(), qt.Equals, "ts")
	c.Assert(storage.GetTtl().GetEnabled().GetTiers(), qt.HasLen, 1)
	c.Assert(storage.GetTtl().GetEnabled().GetTiers()[0].GetApplyAfterSeconds(), qt.Equals, uint64(86400))
	c.Assert(storage.GetTtl().GetEnabled().GetTiers()[0].GetDelete(), qt.IsNotNil)
	c.Assert(records.Facets[0].Kind, qt.Equals, schema.TableProperties)
	values, err := provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	normalized, err := client.NormalizeValues(ctx, values)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateNormalizedInspection(client.Description(), request, snapshot, normalized), qt.IsNil)
	t.Run("host export", func(t *testing.T) { assertHostSchemaExport(t, client, binary, digest, []string{directory}, values) })
}

// TestColumnTableInspectionWithoutMonitoringSubprocess reads the same kind of
// directory through an address that names no monitoring endpoint. The whole
// inspection is refused, and the provider's refusal names the parameter the
// address lacks; the column table is never read as a row table. The SDK client
// hands the host the status code alone, so the message is checked on the
// provider's own reply.
func TestColumnTableInspectionWithoutMonitoringSubprocess(t *testing.T) {
	c := qt.New(t)
	address := withoutMonitoring(c, monitoredAddress(c))
	opened, directory, database := inspectionFixture(c)
	createFixtureTable(c, opened, database, directory)
	createColumnTable(c, opened, path.Join(database, directory, "events"), "(id Uint64 NOT NULL, PRIMARY KEY (id)) WITH (STORE = COLUMN)")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(snapshot, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, `provider inspect failed \(FailedPrecondition\)`)
	direct := backend.New()
	providerSession, err := direct.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		c.Check(direct.Close(context.Background(), &protocol.CloseRequest{SessionId: providerSession.Id}), qt.IsNil)
	})
	snapshot, err = direct.Inspect(provider.ContextWithHostFeatures(ctx, []string{provider.ObservedColumnsFeature}), &protocol.InspectRequest{SessionId: providerSession.Id, Schemas: []string{directory}})
	c.Assert(snapshot, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, `rpc error: code = FailedPrecondition desc = column table `+path.Join(database, directory, "events")+
		` requires monitoring=http\(s\)://host:port in the YDB address: the table service leaves out a column table's shard count and local indexes`)
}

// snakeCase spells a flag the way the configuration does:
// EnableTieringInColumnShard is enable_tiering_in_column_shard.
func snakeCase(name string) string {
	var b strings.Builder
	runes := []rune(name)
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1]) ||
			(i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// clusterFlags is the test server's flag page and a client of its dynamic
// configuration.
type clusterFlags struct {
	opened *connection.Connection
	client Ydb_DynamicConfig_V1.DynamicConfigServiceClient
}

func newClusterFlags(c *qt.C, opened *connection.Connection) *clusterFlags {
	return &clusterFlags{opened: opened, client: Ydb_DynamicConfig_V1.NewDynamicConfigServiceClient(ydb.GRPCConn(opened.Driver))}
}

// page reads the database's flags with what each was explicitly set to.
func (f *clusterFlags) page(c *qt.C) (featureflags.Flags, map[string]bool) {
	c.Helper()
	body, err := f.opened.ReadMonitoring(c.Context(), "feature flags", featureflags.Path, url.Values{"database": {f.opened.Database()}})
	c.Assert(err, qt.IsNil)
	flags, err := featureflags.Decode(body, f.opened.Database())
	c.Assert(err, qt.IsNil)
	var listed struct {
		Databases []struct {
			FeatureFlags []struct {
				Name    string
				Current *bool
			}
		}
	}
	c.Assert(json.Unmarshal(body, &listed), qt.IsNil)
	set := map[string]bool{}
	for _, database := range listed.Databases {
		for _, flag := range database.FeatureFlags {
			if flag.Current != nil {
				set[flag.Name] = *flag.Current
			}
		}
	}
	return flags, set
}

// identity reads the dynamic configuration, which a cluster the test may
// change has none of.
func (f *clusterFlags) identity(ctx context.Context, c *qt.C) *Ydb_DynamicConfig.GetConfigResult {
	c.Helper()
	response, err := f.client.GetConfig(ctx, &Ydb_DynamicConfig.GetConfigRequest{})
	c.Assert(err, qt.IsNil)
	c.Assert(response.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("%v", response.GetOperation().GetIssues()))
	var result Ydb_DynamicConfig.GetConfigResult
	c.Assert(response.GetOperation().GetResult().UnmarshalTo(&result), qt.IsNil)
	return &result
}

// set replaces the cluster's feature flags with every flag the cluster was
// started with and the ones given. A dynamic configuration replaces the flags
// of the static one as a whole, so the flags the cluster was started with are
// written out again.
func (f *clusterFlags) set(c *qt.C, flags map[string]bool) {
	c.Helper()
	before := f.identity(c.Context(), c)
	c.Assert(before.GetConfig(), qt.Equals, "", qt.Commentf("the cluster carries a dynamic configuration of its own, which this test would replace"))
	_, started := f.page(c)
	for name, value := range flags {
		started[name] = value
	}
	var section strings.Builder
	for name, value := range started {
		fmt.Fprintf(&section, "    %s: %t\n", snakeCase(name), value)
	}
	config := fmt.Sprintf("---\nmetadata:\n  kind: MainConfig\n  cluster: %q\n  version: %d\nconfig:\n  feature_flags:\n%sallowed_labels: {}\nselector_config: []\n",
		before.GetIdentity().GetCluster(), before.GetIdentity().GetVersion(), section.String())
	replaced, err := f.client.ReplaceConfig(c.Context(), &Ydb_DynamicConfig.ReplaceConfigRequest{Config: config})
	c.Assert(err, qt.IsNil)
	c.Assert(replaced.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("%v", replaced.GetOperation().GetIssues()))
	c.Cleanup(func() { f.drop(context.Background(), c) })
	f.await(c, flags)
}

// drop removes the dynamic configuration, if one is left.
func (f *clusterFlags) drop(ctx context.Context, c *qt.C) {
	current := f.identity(ctx, c)
	if current.GetConfig() == "" {
		return
	}
	dropped, err := f.client.DropConfig(ctx, &Ydb_DynamicConfig.DropConfigRequest{Identity: current.GetIdentity()})
	c.Assert(err, qt.IsNil)
	c.Assert(dropped.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("%v", dropped.GetOperation().GetIssues()))
}

// await waits until the page reports every flag given with its value.
func (f *clusterFlags) await(c *qt.C, want map[string]bool) {
	c.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		flags, _ := f.page(c)
		matched := true
		for name, value := range want {
			matched = matched && flags[name] == value
		}
		if matched {
			return
		}
		c.Assert(time.Now().Before(deadline), qt.IsTrue, qt.Commentf("the cluster did not report %v within a minute", want))
		time.Sleep(time.Second)
	}
}

// TestFeatureFlagCapabilitySubprocess turns EnableTieringInColumnShard on and
// off on the test server through its dynamic configuration, and reads the
// tiered_ttl capability a new session reports each time.
func TestFeatureFlagCapabilitySubprocess(t *testing.T) {
	c := qt.New(t)
	address := monitoredAddress(c)
	parsed, err := connection.Parse(address)
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, parsed)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	flags := newClusterFlags(c, opened)
	initial, _ := flags.page(c)
	c.Assert(initial["EnableTieringInColumnShard"], qt.IsFalse, qt.Commentf("the test server starts with tiering off"))
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	capability := func() map[string]bool {
		session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
		c.Assert(err, qt.IsNil)
		c.Check(client.CloseSession(ctx, session.Id), qt.IsNil)
		return session.Capabilities
	}
	c.Assert(capability()["tiered_ttl"], qt.IsFalse)
	flags.set(c, map[string]bool{"EnableTieringInColumnShard": true})
	c.Assert(capability()["tiered_ttl"], qt.IsTrue)
	flags.drop(ctx, c)
	flags.await(c, map[string]bool{"EnableTieringInColumnShard": false})
	c.Assert(capability()["tiered_ttl"], qt.IsFalse)
	// Without the endpoint, the session reports the line's default.
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: withoutMonitoring(c, address)})
	c.Assert(err, qt.IsNil)
	c.Assert(client.CloseSession(ctx, session.Id), qt.IsNil)
	c.Assert(session.Capabilities["tiered_ttl"], qt.IsFalse)
}
