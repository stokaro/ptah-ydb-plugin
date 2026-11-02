//go:build integration

package integration_test

import (
	"context"
	"path"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/history"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/ownedtable"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	wire "github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

func TestOwnedHistoryInspectionPreservesForeignTables(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	binary, digest := providerBinary(c)
	client, id, _ := historySession(c, binary, digest)
	for _, format := range []protocol.HistoryScope_Format{protocol.HistoryScope_NATIVE, protocol.HistoryScope_ATLAS} {
		name := "custom_" + format.String()
		scope := &protocol.HistoryScope{SessionId: id, Schema: directory, Table: name, Format: format}
		_, err := client.PrepareHistory(c.Context(), &protocol.PrepareHistoryRequest{Scope: scope, RequestId: name})
		c.Assert(err, qt.IsNil)
		c.Cleanup(func() {
			_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+path.Join(database, directory, name)+"`")
			c.Check(err, qt.IsNil)
		})
	}
	// Identical native history layout and a conventional name are not ownership.
	foreign := path.Join(database, directory, "ptah_schema_revisions")
	_, err := opened.DB.ExecContext(c.Context(), "CREATE TABLE `"+foreign+"` (version Int64 NOT NULL, revision String NOT NULL, PRIMARY KEY(version))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+foreign+"`")
		c.Check(err, qt.IsNil)
	})
	request := &protocol.InspectRequest{SessionId: id, Schemas: []string{directory}}
	snapshot, err := client.Inspect(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 1)
	c.Assert(snapshot.Tables[0].Identity.Name, qt.Equals, "ptah_schema_revisions")
	c.Assert(snapshot.ReservedObjects, qt.HasLen, 2)
	for _, reserved := range snapshot.ReservedObjects {
		c.Assert(reserved.Purpose, qt.Equals, protocol.ReservedObject_HISTORY)
		c.Assert(reserved.Object.Schema, qt.Equals, directory)
		c.Assert(reserved.Object.Database, qt.Equals, database)
		c.Assert(reserved.Object.Kind, qt.Equals, "table")
		assertReservedCoverage(c, snapshot, reserved)
	}
	_, err = provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	root, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: id, Schemas: []string{"/"}})
	c.Assert(err, qt.IsNil)
	c.Assert(root.ReservedObjects, qt.HasLen, 0)
}

func assertReservedCoverage(c *qt.C, snapshot *protocol.Schema, reserved *protocol.ReservedObject) {
	c.Helper()
	expected := []string{"table"}
	for _, model := range schema.ObservedModels() {
		if !schema.IsNativeObject(model.Kind) {
			continue
		}
		expected = append(expected, model.Kind)
		matches := 0
		for _, subject := range snapshot.Coverage.Subjects {
			if subject.Kind == model.Kind && subject.Subject.Name == reserved.Object.Name && subject.Subject.Schema == reserved.Object.Schema {
				matches++
				c.Assert(subject.Knowledge.State, qt.Equals, protocol.ModelKnowledge_UNREPRESENTABLE)
				c.Assert(subject.Knowledge.Reason, qt.Not(qt.Equals), "")
			}
		}
		c.Assert(matches, qt.Equals, 1)
	}
	c.Assert(reserved.ConflictKinds, qt.DeepEquals, expected)
}

func TestAlteredHistoryRefusesInspectionBeforeChildProjection(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	owner, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(owner.Close(), qt.IsNil) })
	store := history.Store{Identity: ownedtable.Identity{Database: database, Directory: directory, Name: "marked_with_index"}, Format: protocol.HistoryScope_NATIVE}
	request := store.CreateRequest()
	request.Indexes = []*Ydb_Table.TableIndex{{Name: "extra", IndexColumns: []string{"revision"}, Type: &Ydb_Table.TableIndex_GlobalIndex{GlobalIndex: &Ydb_Table.GlobalIndex{}}}}
	c.Assert(owner.Create(c.Context(), request), qt.IsNil)
	badPath := store.Path()
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+badPath+"`")
		c.Check(err, qt.IsNil)
	})
	snapshot, err := inspect.Read(c.Context(), opened.Driver, columnTables(opened), database, []string{directory})
	c.Assert(err, qt.ErrorMatches, ".*storage has incompatible layout.*")
	c.Assert(snapshot, qt.IsNil)
	// Unknown marker versions must not fall back to an ordinary managed table.
	store.Name = "unknown_marker"
	request = store.CreateRequest()
	request.Attributes["ptah.history"] = "stokaro/ptah-ydb-plugin/history-v99/native"
	c.Assert(owner.Create(c.Context(), request), qt.IsNil)
	unknown := store.Path()
	c.Cleanup(func() {
		_, err := opened.DB.ExecContext(context.Background(), "DROP TABLE `"+unknown+"`")
		c.Check(err, qt.IsNil)
	})
	// Direct description isolates this refusal from the earlier malformed table.
	description, err := owner.Describe(c.Context(), unknown)
	c.Assert(err, qt.IsNil)
	owned, err := history.Owned(store.Name, description)
	c.Assert(err, qt.ErrorMatches, "unknown YDB history ownership marker")
	c.Assert(owned, qt.IsFalse)
}

// A repeated create returns SUCCESS, so a creation receipt does not show that
// the node is one the provider made. The lock service checks the settings of
// the node it finds instead.
func TestLockNodeCreateSuccessDoesNotProveOwnership(t *testing.T) {
	c := testcheck.New(t)
	opened, _, database := lockFixture(c)
	service := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(opened.Driver))
	request := &wire.CreateNodeRequest{Path: path.Join(database, locking.NodeName), Config: &wire.Config{
		ReadConsistencyMode:   wire.ConsistencyMode_CONSISTENCY_MODE_STRICT,
		AttachConsistencyMode: wire.ConsistencyMode_CONSISTENCY_MODE_STRICT,
	}}
	first, err := service.CreateNode(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(nativeapi.Status(first.GetOperation()), qt.IsNil)
	second, err := service.CreateNode(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(second.GetOperation().GetReady(), qt.IsTrue)
	c.Assert(second.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
}

// The lock node at the root is reserved operational storage: a read reports
// its name as taken and describes no node there, so an export of the read
// declares nothing the declarations refuse.
func TestLockNodeIsReservedOperationalStorage(t *testing.T) {
	c := testcheck.New(t)
	_, _, database := lockFixture(c)
	binary, digest := providerBinary(c)
	client, id, _ := historySession(c, binary, digest)
	acquireNativeLock(c, client, nativeLockRequest(id, "held", "migration"))
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: id, Schemas: []string{"/"}})
	c.Assert(err, qt.IsNil)
	// Other objects may sit at the root of a shared server; none of them is
	// the lock node.
	c.Assert(slices.ContainsFunc(snapshot.NativeObjects, func(value *protocol.Envelope) bool { return value.GetOwner().GetName() == locking.NodeName }), qt.IsFalse)
	c.Assert(snapshot.ReservedObjects, qt.HasLen, 1)
	c.Assert(snapshot.ReservedObjects[0].Purpose, qt.Equals, protocol.ReservedObject_LOCK)
	c.Assert(snapshot.ReservedObjects[0].Object.Name, qt.Equals, locking.NodeName)
	c.Assert(snapshot.ReservedObjects[0].Object.Database, qt.Equals, database)
	c.Assert(snapshot.ReservedObjects[0].Object.Kind, qt.Equals, schema.CoordinationProperties)
	inputs, err := provider.InspectionValues(client.Description(), &protocol.InspectRequest{SessionId: id, Schemas: []string{"/"}}, snapshot)
	c.Assert(err, qt.IsNil)
	t.Run("host export", func(t *testing.T) { assertHostSchemaExport(t, client, binary, digest, []string{"/"}, inputs) })
}
