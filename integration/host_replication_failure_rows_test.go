//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from TestGenerateMigrationAST_Replications_FailurePath: the schema
// cannot change a replication's consistency level in place. A refusal must
// leave the running replication and its replica intact.
func TestHostRefusesToChangeReplicationConsistencyLevel_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostReplication(c)
	initial := h.tables("src") + h.mirror
	created, err := h.apply(h.tables("src"))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	created, err = h.apply(initial)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	h.awaitReplica()
	before, state, found := h.replication()
	c.Assert(found, qt.IsTrue)
	c.Assert(state, qt.Equals, replication.StateRunning)

	changed := h.tables("src") + strings.Replace(h.mirror, "    items:\n", "    consistency_level: global\n    items:\n", 1)
	refused, err := h.apply(changed)
	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Contains, "its consistency_level differ from the database's")
	after, state, found := h.replication()
	c.Assert(found, qt.IsTrue)
	c.Assert(state, qt.Equals, replication.StateRunning)
	c.Assert(after.ConsistencyLevel, qt.Equals, before.ConsistencyLevel)
	c.Assert(replicaWritten(c, c.Context(), h.opened, path.Join(h.database, h.directory), "rep"), qt.IsTrue)
}

// Ported from TestGenerateMigrationAST_Replications_FailurePath: YDB changes
// no transfer target in place. A rejected declaration must not create its new
// target table or alter the existing transfer.
func TestHostRefusesToRetargetATransfer_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	h := newHostTransfer(c)
	topic := "topics:\n  events:\n    schema: " + h.directory + "\n"
	initial := h.orderLog() + topic + h.transfer(lambdaWriting("a:"), "")
	created, err := h.apply(initial)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	before, _, found := h.transferRead()
	c.Assert(found, qt.IsTrue)
	other := strings.Replace(h.orderLog(), "tables:\n  order_log:", "  other_log:", 1)
	retargeted := strings.Replace(h.transfer(lambdaWriting("a:"), ""), path.Join(h.directory, "order_log"), path.Join(h.directory, "other_log"), 1)

	refused, err := h.apply(h.orderLog() + other + topic + retargeted)
	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Contains, "its target differ from the database's")
	after, _, found := h.transferRead()
	c.Assert(found, qt.IsTrue)
	c.Assert(after.Target, qt.Equals, before.Target)
	c.Assert(listedNames(c, h.opened, path.Join(h.database, h.directory)), qt.Not(qt.Contains), "other_log")
}
