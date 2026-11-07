package inspect

// White-box testing required: foreignReplication runs only inside
// ReadObjects, which opens a table session on a YDB driver before it reads a
// changefeed, so no offline caller reaches it. It lists through the scheme
// client alone, which a fake answers here. The live tests
// TestHostRefusesToDropATableAReplicationOnAnotherServerReads_FailurePath and
// TestHostRefusesToDropATableAReplicationOfANestedDatabaseReads_FailurePath
// drive the public read and plan against a server.
import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/types/known/anypb"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// replicaTree is the database /local holding the nested database db2 and
// the directory app, which holds a row table, a column table, a topic and the
// empty directory sub.
func replicaTree(c *qt.C) *schemeTree {
	c.Helper()
	tree := &schemeTree{answers: make(map[string]*Ydb_Operations.Operation)}
	for absolute, listing := range map[string]*Ydb_Scheme.ListDirectoryResult{
		"/local": {Self: &Ydb_Scheme.Entry{Name: "local", Type: Ydb_Scheme.Entry_DATABASE}, Children: []*Ydb_Scheme.Entry{
			{Name: "db2", Type: Ydb_Scheme.Entry_DATABASE}, {Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY},
		}},
		"/local/app": {Self: &Ydb_Scheme.Entry{Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}, Children: []*Ydb_Scheme.Entry{
			{Name: "rep", Type: Ydb_Scheme.Entry_TABLE}, {Name: "columns", Type: Ydb_Scheme.Entry_COLUMN_TABLE},
			{Name: "events", Type: Ydb_Scheme.Entry_TOPIC}, {Name: "sub", Type: Ydb_Scheme.Entry_DIRECTORY},
		}},
		"/local/app/sub": {Self: &Ydb_Scheme.Entry{Name: "sub", Type: Ydb_Scheme.Entry_DIRECTORY}},
	} {
		payload, err := anypb.New(listing)
		c.Assert(err, qt.IsNil)
		tree.answers[absolute] = &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: payload}
	}
	return tree
}

// boundFeed is a changefeed whose replication binding names the replica at
// replica.
func boundFeed(replica string) *native.ChangefeedDescription {
	return &native.ChangefeedDescription{Name: "updates", Attributes: map[string]string{
		"__async_replication": `{"path":"` + replica + `","id":"1","supports_topic_autopartitioning":false}`,
	}}
}

// The read says why the replication that controls a stream lives in another
// database where the replica its binding names lies below the root: in a
// nested database, or at a path that holds no table here. A replica at a
// table of this database, a replica outside the root and a changefeed with no
// binding carry no reason, and the read lists nothing for the last two.
func TestForeignReplicationSaysWhereTheReplicaLies(t *testing.T) {
	const absent = "this database holds no table at that path, so a database of the same name on another server holds the replica"
	for _, test := range []struct {
		name       string
		feed       *native.ChangefeedDescription
		want       string
		wantListed []string
	}{
		{name: "no binding", feed: &native.ChangefeedDescription{Name: "updates"}},
		{name: "a replica outside the root", feed: boundFeed("/other/app/rep")},
		{name: "a replica at a sibling of the root", feed: boundFeed("/localother/rep")},
		{name: "a row table of this database", feed: boundFeed("/local/app/rep"), wantListed: []string{"/local", "/local/app"}},
		{name: "a column table of this database", feed: boundFeed("/local/app/columns"), wantListed: []string{"/local", "/local/app"}},
		{name: "a nested database", feed: boundFeed("/local/db2/app/rep"), want: "the replica lies in the database /local/db2, which is nested in this one",
			wantListed: []string{"/local"}},
		{name: "no entry at the path", feed: boundFeed("/local/app/gone"), want: absent, wantListed: []string{"/local", "/local/app"}},
		{name: "no directory on the way", feed: boundFeed("/local/gone/rep"), want: absent, wantListed: []string{"/local"}},
		{name: "an empty directory on the way", feed: boundFeed("/local/app/sub/rep"), want: absent, wantListed: []string{"/local", "/local/app", "/local/app/sub"}},
		{name: "a topic at the path", feed: boundFeed("/local/app/events"), want: absent, wantListed: []string{"/local", "/local/app"}},
		{name: "a topic on the way", feed: boundFeed("/local/app/events/rep"), want: absent, wantListed: []string{"/local", "/local/app"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			tree := replicaTree(c)
			read := &source{scheme: tree}

			got, err := read.foreignReplication(c.Context(), "/local", test.feed)

			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
			c.Assert(tree.listed, qt.DeepEquals, test.wantListed)
		})
	}
}
