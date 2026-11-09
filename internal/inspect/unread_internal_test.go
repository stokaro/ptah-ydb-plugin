package inspect

// White-box testing required: unreadReplicas and snapshotTargets are called
// only by the directory walk, so a public read reaches them only through a
// server holding a replica table, and the live host tests cover that path on
// both release lines.
import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

const orphanReason = "dir.rep is a replica table no replication of this database writes any more, which YDB keeps read-only " +
	"for good (`path is an async replica table`); drop it by hand with DROP TABLE, which YDB accepts, or remove it from the schema"

// replicaRef is the replica table dir/rep the walk found.
func replicaRef() *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "dir", Kind: "table", Name: "rep"}
}

func TestUnreadReplicas_HappyPath(t *testing.T) {
	t.Run("a replica no replication writes, every replication read", func(t *testing.T) {
		c := testcheck.New(t)
		unread := unreadReplicas([]*protocol.Reference{replicaRef()}, replicationTargets{paths: []string{"other"}, complete: true})
		c.Assert(unread, qt.HasLen, 1)
		c.Assert(proto.Equal(unread[0].GetTable(), replicaRef()), qt.IsTrue)
		c.Assert(unread[0].GetDiagnostic().GetReason(), qt.Equals, orphanReason)
	})
	t.Run("a replica of a replication the read could not describe", func(t *testing.T) {
		c := testcheck.New(t)
		unread := unreadReplicas([]*protocol.Reference{replicaRef()}, replicationTargets{})
		c.Assert(unread, qt.HasLen, 1)
		c.Assert(unread[0].GetDiagnostic().GetReason(), qt.Equals, "dir.rep is a replica table of an async replication this read "+
			"cannot describe, and YDB keeps a replica table read-only (`path is an async replica table`); remove it from the schema")
	})
	t.Run("a name that would make the reason too long", func(t *testing.T) {
		c := testcheck.New(t)
		ref := replicaRef()
		ref.Name = strings.Repeat("r", 300)
		unread := unreadReplicas([]*protocol.Reference{ref}, replicationTargets{complete: true})
		c.Assert(unread, qt.HasLen, 1)
		c.Assert(unread[0].GetDiagnostic().GetReason(), qt.Equals, strings.Replace(orphanReason, "dir.rep", "it", 1))
	})
}

// A replica a replication writes, at its own path or in a directory the
// replication targets, is the replication's and not named.
func TestUnreadReplicas_WrittenByAReplication(t *testing.T) {
	for _, target := range []string{"dir/rep", "dir"} {
		t.Run(target, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(unreadReplicas([]*protocol.Reference{replicaRef()}, replicationTargets{paths: []string{target}, complete: true}), qt.HasLen, 0)
		})
	}
}

// The targets of a snapshot are the items of the replications it holds, and
// nothing else it holds.
func TestSnapshotTargets(t *testing.T) {
	c := testcheck.New(t)
	spec := replication.ReplicationSpec{Connection: replication.Connection{ConnectionString: "grpc://localhost:2136/?database=/local"},
		Items: []replication.Item{{Source: "src", Target: "dir/rep"}, {Source: "more", Target: "dir/more"}}}
	value, err := schema.CaptureObject(&protocol.Reference{Engine: "ydb", Database: "/local", Kind: schema.ReplicationProperties, Name: "mirror"},
		replication.ObservedReplication(spec, "running"))
	c.Assert(err, qt.IsNil)
	targets, err := snapshotTargets(&protocol.Schema{NativeObjects: []*protocol.Envelope{value}})
	c.Assert(err, qt.IsNil)
	c.Assert(targets, qt.DeepEquals, []string{"dir/rep", "dir/more"})
}
