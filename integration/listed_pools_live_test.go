//go:build integration

package integration_test

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A database's scheme lists its resource pools under
// .metadata/workload_manager/pools, the server's default pool among them, and
// a read of the whole database reads a pool the listing names with the
// settings its system view reports, recording none as unrepresentable. A pool
// listed with no system-view row is recorded unrepresentable, as the host's
// embedded engine recorded it; no server state makes such a pool, so
// TestWorkload_RecordsAListedPoolTheSystemViewMisses pins that half, and this
// test is its control on a server.
func TestInspectionReadsTheListedPools(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	enableResourcePools(c, opened)
	pool := directory + "_listed"
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE RESOURCE POOL `"+pool+"` WITH (CONCURRENT_QUERY_LIMIT = 3)"), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP RESOURCE POOL `"+pool+"`")
	})
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	c.Assert(session.Capabilities[workload.CapabilityResourcePools], qt.IsTrue, qt.Commentf("the live target needs EnableResourcePools and its monitoring endpoint"))

	listed, err := nativeapi.ListedPools(c.Context(), nativeapi.SchemeLister(Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(opened.Driver))), database)
	c.Assert(err, qt.IsNil)
	read, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{"/"}})
	c.Assert(err, qt.IsNil)

	c.Assert(listed, qt.Contains, "default")
	c.Assert(listed, qt.Contains, pool)
	c.Assert(slices.ContainsFunc(read.GetNativeObjects(), func(value *protocol.Envelope) bool {
		return value.GetKind() == schema.ResourcePoolProperties && value.GetOwner().GetName() == pool
	}), qt.IsTrue)
	c.Assert(slices.ContainsFunc(read.GetCoverage().GetSubjects(), func(subject *protocol.ModelSubjectCoverage) bool {
		return subject.GetKind() == schema.ResourcePoolProperties && subject.GetKnowledge().GetState() == protocol.ModelKnowledge_UNREPRESENTABLE
	}), qt.IsFalse)
}
