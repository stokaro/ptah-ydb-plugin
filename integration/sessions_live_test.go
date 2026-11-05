//go:build integration

package integration_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
)

// hostFeatures are the features startProvider's host negotiates.
func hostFeatures() []string {
	return []string{provider.ObservedColumnsFeature, provider.ObservedIndexesFeature, provider.PlanningLifecycleFeature, provider.DatabaseContextFeature, provider.ValueContextFeature}
}

// negotiated are the engine features client negotiated with the provider,
// which a host names on every request. A request that carries what a session
// read has to name them: once value context is negotiated, inspection binds
// each value to the database it was read in, and the SDK refuses a value whose
// context the request's target features do not name.
func negotiated(client *providerruntime.Client) []string {
	return client.Description().GetEngines()[0].GetFeatures()
}

func startProvider(c *qt.C, binary, digest string) *providerruntime.Client {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{
		Executable: binary, SHA256: digest, ProviderID: "github.com/stokaro/ptah-ydb-plugin",
		HostFeatures: hostFeatures(),
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	return client
}

func TestNativeSessionsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	c.Assert(address, qt.Not(qt.Equals), "", qt.Commentf("PTAH_YDB_TEST_URL must select a live YDB test database"))
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	first, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Assert(first.GetDatabase(), qt.Equals, "/local")
	c.Assert(first.GetServerVersion(), qt.Not(qt.Equals), "")
	c.Logf("native YDB version: %s", first.GetServerVersion())
	second, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Assert(second.GetId(), qt.Not(qt.Equals), first.GetId())
	c.Assert(second.GetServerVersion(), qt.Equals, first.GetServerVersion())
	c.Assert(client.CloseSession(ctx, first.GetId()), qt.IsNil)
	c.Assert(client.CloseSession(ctx, first.GetId()), qt.IsNil)
	// Closing a handle does not prevent another session from opening.
	third, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Assert(client.CloseSession(ctx, second.GetId()), qt.IsNil)
	other := startProvider(c, binary, digest)
	c.Assert(other.CloseSession(ctx, first.GetId()), qt.ErrorMatches, `.*\(NotFound\)`)
	c.Assert(client.CloseSession(ctx, "missing"), qt.ErrorMatches, `.*\(NotFound\)`)
	_, err = client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: "ydb://alice:test-secret@host/local?token=other"})
	c.Assert(err, qt.IsNotNil)
	c.Assert(err.Error(), qt.Not(qt.Contains), "test-secret")
	c.Assert(err.Error(), qt.Not(qt.Contains), "other")
	// A closed session cannot inspect through its previously opened driver.
	_, err = client.Inspect(ctx, &protocol.InspectRequest{SessionId: first.GetId()})
	c.Assert(err, qt.ErrorMatches, `.*\(FailedPrecondition\)`)
	c.Assert(client.CloseSession(ctx, third.GetId()), qt.IsNil)
	// A new process does not accept old handles even when the database is equal.
	live, err := other.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Assert(live.GetId(), qt.Not(qt.Equals), first.GetId())
	c.Assert(other.CloseSession(ctx, live.GetId()), qt.IsNil)
}

func TestNativeSessionConcurrentClose(t *testing.T) {
	c := testcheck.New(t)
	address := os.Getenv("PTAH_YDB_TEST_URL")
	c.Assert(address, qt.Not(qt.Equals), "")
	service := backend.New()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	session, err := service.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	// The backend releases native resources even after the request was canceled.
	canceled, stop := context.WithCancel(ctx)
	stop()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { errs[i] = service.Close(canceled, &protocol.CloseRequest{SessionId: session.GetId()}) })
	}
	wg.Wait()
	c.Assert(errs, qt.DeepEquals, make([]error, 8))
	refused, err := service.Open(canceled, &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(refused, qt.IsNil)
	c.Assert(err, qt.IsNotNil)
}

func TestNativeConnectionReleasesBothOwners(t *testing.T) {
	c := testcheck.New(t)
	address, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	_, err = opened.Driver.Scheme().ListDirectory(ctx, address.Database())
	c.Assert(err, qt.IsNil)
	other, err := connection.Open(ctx, address)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(other.Close(), qt.IsNil) })
	c.Assert(opened.Close(), qt.IsNil)
	var version string
	c.Assert(other.DB.QueryRowContext(ctx, "SELECT Version()").Scan(&version), qt.IsNil)
	c.Assert(version, qt.Not(qt.Equals), "")
	c.Assert(opened.DB.PingContext(ctx), qt.IsNotNil)
	_, err = opened.Driver.Scheme().ListDirectory(ctx, address.Database())
	c.Assert(err, qt.IsNotNil)
	c.Assert(opened.Close(), qt.IsNil)
}

// lineCapabilities are the capabilities a session reports on each line the
// test servers run, for a host that lists index renames: the line's preset
// with the keys the servers' feature flags turn on, which the session reads
// from the monitoring endpoint the test URL names. Both servers start with
// enable_vector_index and enable_external_data_sources, and 26.2 also with
// enable_add_unique_index and enable_streaming_queries.
var lineCapabilities = map[string]map[string]bool{
	"26.2": {
		"transactional_ddl": false, "ddl_inside_transaction": false, "secrets": true, "coordination_nodes": true, "migration_statement_timeout": true, "tiered_ttl": false,
		"role_management": true, "role_membership": true, "group_principals": true, "database_grants": true, "relative_grant_paths": true,
		"topics": true, "topic_consumer_availability_period": true,
		"changefeeds": true, "changefeed_user_sids": true, "changefeed_schema_changes": true, "changefeed_topic_auto_partitioning": true,
		// enable_external_data_sources.
		"external_data_sources": true, "external_object_replace": false, "external_data_source_secret_paths": true,
		// enable_streaming_queries, with enable_external_data_sources.
		"resource_pools": false, "streaming_queries": true,
		"async_replication": true, "transfers": true, "replication_secret_paths": true,
		"wide_date_time_types": true, "parameterized_decimal": true, "alter_column_default": true, "add_column_with_default": true, "small_integer_defaults": true, "document_type_defaults": true,
		"vector_indexes": true, "vector_index_maintained_on_write": true, "vector_bit_type": true, "full_text_indexes": true,
		"local_bloom_indexes": true, "local_ngram_indexes": true, "local_min_max_indexes": false,
		// enable_add_unique_index.
		"serial_columns": true, "serial_sequence_options": true, "serial_sequence_keeps_range": false, "unique_constraints": false, "unique_index_on_existing_table": true, "index_rename": true,
		"returning_clause": true, "common_table_expressions": false, "correlated_subqueries": false, "non_equi_joins": false, "offset_without_limit": false,
		"column_family_cache_mode": true,
	},
	"25.1": {
		"transactional_ddl": false, "ddl_inside_transaction": false, "secrets": false, "coordination_nodes": true, "migration_statement_timeout": true, "tiered_ttl": false,
		"role_management": true, "role_membership": true, "group_principals": true, "database_grants": true, "relative_grant_paths": false,
		"topics": true, "topic_consumer_availability_period": false,
		"changefeeds": true, "changefeed_user_sids": false, "changefeed_schema_changes": false, "changefeed_topic_auto_partitioning": false,
		// enable_external_data_sources.
		"external_data_sources": true, "external_object_replace": false, "external_data_source_secret_paths": false,
		"resource_pools": false, "streaming_queries": false,
		"async_replication": true, "transfers": false, "replication_secret_paths": false,
		"wide_date_time_types": false, "parameterized_decimal": false, "alter_column_default": false, "add_column_with_default": false, "small_integer_defaults": false, "document_type_defaults": false,
		// enable_vector_index.
		"vector_indexes": true, "vector_index_maintained_on_write": false, "vector_bit_type": false, "full_text_indexes": false,
		"local_bloom_indexes": false, "local_ngram_indexes": false, "local_min_max_indexes": false,
		"serial_columns": true, "serial_sequence_options": true, "serial_sequence_keeps_range": false, "unique_constraints": false, "unique_index_on_existing_table": false, "index_rename": true,
		"returning_clause": false, "common_table_expressions": false, "correlated_subqueries": false, "non_equi_joins": false, "offset_without_limit": false,
		"column_family_cache_mode": false,
	},
}

// Ported from the host's TestYDBConnection_DescribesTheServer: a session
// reports the server it reached, the version Version() answers, the database
// root and no default schema, and the whole capability map of the line that
// version is on. A test URL that reaches a server on another line, or a
// server started with other flags, fails here rather than letting the rest of
// the suite measure that line under this one's name.
func TestNativeSessionDescribesTheServer_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, _, _ := inspectionFixture(c)
	client := startProviderFor(c, provider.ObservedIndexesFeature, provider.PlanningLifecycleFeature, provider.DatabaseContextFeature, provider.ValueContextFeature,
		provider.IndexRenamesFeature)

	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})

	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.GetId()), qt.IsNil) })
	version := serverVersion(c, opened)
	c.Assert(session.GetServerVersion(), qt.Equals, version)
	c.Assert(session.GetDatabase(), qt.Equals, "/local")
	c.Assert(session.GetDefaultSchema(), qt.Equals, "")
	c.Assert(session.GetCapabilities(), qt.DeepEquals, lineCapabilities[serverLine(c, version)])
}
