//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-sdk/v3/coordination"
	"google.golang.org/protobuf/encoding/protojson"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// realmNodes is what a read of a session reports of its coordination nodes:
// each node's directory and name, and its attach consistency mode.
func realmNodes(c *qt.C, snapshot *protocol.Schema) map[string]Ydb_Coordination.ConsistencyMode {
	c.Helper()
	nodes := make(map[string]Ydb_Coordination.ConsistencyMode)
	for _, object := range snapshot.GetNativeObjects() {
		if object.GetKind() != schema.CoordinationProperties {
			continue
		}
		var value Ydb_Coordination.DescribeNodeResult
		c.Assert(protojson.Unmarshal(object.GetPayload(), &value), qt.IsNil)
		nodes[path.Join(object.GetOwner().GetSchema(), object.GetOwner().GetName())] = value.GetConfig().GetAttachConsistencyMode()
	}
	return nodes
}

// Ported from the host's TestYDBCoordinationNodes_InADevRealm: a node a dev
// realm's session creates by a relative path lands under the realm, holds the
// settings the statement gives it, and the realm's read reports it by its
// path in the realm. The host's test also creates a node named ptah_locks at
// the realm's root and reads it as an ordinary node; this provider reserves
// that name at the root of every session's database, a realm's included,
// for its schema lock (TestDevCleanupSubprocess).
func TestRealmSessionCreatesAndReadsANode_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c)
	session, root, opened := realmSession(c, client)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		emptyTree(cleanup, opened, root)
	})

	created := executeSQL(c, client, session, "", "CREATE COORDINATION NODE `app/locks` WITH (attach_consistency_mode = 'relaxed')")
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session})

	c.Assert(created.Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(heldNode(c, opened, path.Join(root, "app", "locks")), qt.DeepEquals, coordination.NodeConfig{AttachConsistencyMode: coordination.ConsistencyModeRelaxed})
	c.Assert(err, qt.IsNil)
	c.Assert(realmNodes(c, snapshot), qt.DeepEquals, map[string]Ydb_Coordination.ConsistencyMode{
		"app/locks": Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_RELAXED,
	})
}

// Ported from the host's TestYDBCoordinationNodes_InADevRealm: a node whose
// relative path climbs out of the realm is refused before it runs, and no
// node is made beside the realms. The host's engine refuses it with its own
// words; this provider refuses every statement that leaves the realm with
// the diagnostic ydb.realm-escape.
func TestRealmSessionRefusesANodeOutsideTheRealm_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderFor(c)
	session, root, opened := realmSession(c, client)
	escaped := path.Join(path.Dir(root), "escaped")
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// The node exists only when the refusal failed; the drop is cleanup,
		// and the assertion below reads the directory before it runs.
		_ = opened.Driver.Coordination().DropNode(cleanup, escaped)
	})

	refused := executeSQL(c, client, session, "", "CREATE COORDINATION NODE `../escaped`")

	c.Assert(refused.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(refused.DiagnosticCode, qt.Equals, "ydb.realm-escape")
	c.Assert(listedNames(c, opened, path.Dir(root)), qt.Not(qt.Contains), "escaped")
}
