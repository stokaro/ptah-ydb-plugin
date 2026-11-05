//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
)

// renamingColumnTableFixture is newColumnTableFixture with a session of a host
// that lists planning.index-renames, which reports index_rename where the
// cluster renames an index in place.
func renamingColumnTableFixture(c *qt.C) *columnTableFixture {
	c.Helper()
	address := monitoredAddress(c)
	fixture := newColumnTableFixture(c, address)
	binary, digest := providerBinary(c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{Executable: binary, SHA256: digest, ProviderID: "github.com/stokaro/ptah-ydb-plugin",
		HostFeatures: append(hostFeatures(), provider.IndexRenamesFeature)})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	fixture.client, fixture.session = client, session
	return fixture
}

// A local index whose name alone changes is renamed in place with one RENAME
// INDEX on a session with index_rename, as the host's embedded engine plans
// it, and keeps the rows it was built over: the table reads back with the
// index under its new name and every setting it had, and nothing is left to
// plan. Gap G82 of stokaro/ptah#4204: the provider planned a drop and an add,
// since the host compares no local index and so pairs none.
func TestColumnTableLocalIndexRenamesInPlaceSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := renamingColumnTableFixture(c)
	requireSessionCapability(c, fixture.session, tablecreate.LocalBloomIndexesCapability, true)
	requireSessionCapability(c, fixture.session, tablecreate.LocalNgramIndexesCapability, true)
	c.Assert(fixture.session.Capabilities[tablecreate.IndexRenameCapability], qt.IsTrue)
	execution, err := fixture.create(c, fixture.session, "create-local-indexes", localIndexStorage(localBloom, localNgram))
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	absolute := path.Join(fixture.database, fixture.directory, "records")
	opened := fixture.connection(c)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "UPSERT INTO `"+absolute+"` (ts, id, body) VALUES (Timestamp('2026-01-01T00:00:00Z'), 1ul, 'one'u)"), qt.IsNil)
	renamed := `{"name":"renamed_bloom","columns":["body"],"bloom":{"falsePositiveProbability":0.01}}`
	storage := localIndexStorage(renamed, localNgram)

	plan, execution, err := fixture.change(c, "local-rename", storage)

	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	c.Assert(stepNames(plan), qt.DeepEquals, []string{"column-table-rename-local-index/000000"})
	fixture.requireLocalIndexes(c, renamed, localNgram)
	c.Assert(fixture.compare(c, storage).Changes, qt.HasLen, 0)
	c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(1))
}
