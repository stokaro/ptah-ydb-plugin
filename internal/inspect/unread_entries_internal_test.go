package inspect

// White-box testing required: the directory walk runs only inside
// ReadObjects, which opens a table session on a YDB driver before it lists
// anything, so no offline caller reaches it. The walk lists through the scheme
// client and describes a coordination node through the coordination client,
// which fakes answer here.
import (
	"context"
	"errors"
	"fmt"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// listings is a scheme tree whose directories, by absolute path, hold the
// entries given: /local is the database, and every other path a directory.
func listings(c *qt.C, directories map[string][]*Ydb_Scheme.Entry) *schemeTree {
	c.Helper()
	tree := &schemeTree{answers: make(map[string]*Ydb_Operations.Operation)}
	for absolute, children := range directories {
		self := &Ydb_Scheme.Entry{Name: path.Base(absolute), Type: Ydb_Scheme.Entry_DIRECTORY}
		if absolute == "/local" {
			self.Type = Ydb_Scheme.Entry_DATABASE
		}
		payload, err := anypb.New(&Ydb_Scheme.ListDirectoryResult{Self: self, Children: children})
		c.Assert(err, qt.IsNil)
		tree.answers[absolute] = &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: payload}
	}
	return tree
}

// describedNodes answers DescribeNode with the description it holds for a
// path. A call other than DescribeNode reaches the embedded nil interface and
// panics.
type describedNodes struct {
	Ydb_Coordination_V1.CoordinationServiceClient
	nodes map[string]*Ydb_Coordination.DescribeNodeResult
}

func (d *describedNodes) DescribeNode(_ context.Context, request *Ydb_Coordination.DescribeNodeRequest, _ ...grpc.CallOption) (*Ydb_Coordination.DescribeNodeResponse, error) {
	payload, err := anypb.New(d.nodes[request.GetPath()])
	if err != nil {
		return nil, err
	}
	return &Ydb_Coordination.DescribeNodeResponse{Operation: &Ydb_Operations.Operation{Ready: true, Status: Ydb.StatusIds_SUCCESS, Result: payload}}, nil
}

// unknownKnowledge describes each subject a read records as anything but
// complete, as its kind, its name, its state and the reason.
func unknownKnowledge(result *protocol.Schema) []string {
	var described []string
	for _, subject := range result.GetCoverage().GetSubjects() {
		if subject.GetKnowledge().GetState() != protocol.ModelKnowledge_COMPLETE {
			described = append(described, fmt.Sprintf("%s %s %s: %s", subject.GetKind(), subject.GetSubject().GetName(),
				subject.GetKnowledge().GetState(), subject.GetKnowledge().GetReason()))
		}
	}
	return described
}

// nativeNames names each native object of a read as its kind and its name.
func nativeNames(result *protocol.Schema) []string {
	var names []string
	for _, value := range result.GetNativeObjects() {
		names = append(names, value.GetKind()+" "+value.GetOwner().GetName())
	}
	return names
}

// unreadNames names each unread table of a read with its reason.
func unreadNames(result *protocol.Schema) []string {
	var names []string
	for _, unread := range result.GetUnreadTables() {
		names = append(names, path.Join(unread.GetTable().GetSchema(), unread.GetTable().GetName())+": "+unread.GetDiagnostic().GetReason())
	}
	return names
}

// A column store and a sequence, which Ptah does not model, are recorded and
// the read goes on, as the host's embedded engine recorded them as not
// described: a host that takes unread tables is told of each, in the
// directory that holds it. A system view outside a dot-directory is the
// server's and is left out. Ported from the host's
// TestReader_RecordsWhatItDoesNotDescribe.
func TestWalkRecordsWhatItDoesNotDescribe(t *testing.T) {
	c := testcheck.New(t)
	read := nestedTraversal(listings(c, map[string][]*Ydb_Scheme.Entry{
		"/local": {{Name: "olap", Type: Ydb_Scheme.Entry_COLUMN_STORE}, {Name: "health", Type: Ydb_Scheme.Entry_SYS_VIEW},
			{Name: "key", Type: Ydb_Scheme.Entry_SECRET}, {Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}},
		"/local/app": {{Name: "seq", Type: Ydb_Scheme.Entry_SEQUENCE}},
	}))
	read.common.UnreadTables = true

	directories, err := read.directory(c.Context(), "")
	c.Assert(err, qt.IsNil)
	_, err = read.directory(c.Context(), "app")
	c.Assert(err, qt.IsNil)
	c.Assert(read.unreadTables(c.Context(), nil), qt.IsNil)

	c.Assert(directories, qt.DeepEquals, []string{"app"})
	c.Assert(nativeNames(read.result), qt.DeepEquals, []string{schema.SecretProperties + " key"})
	c.Assert(unreadNames(read.result), qt.DeepEquals, []string{
		"olap: olap is a column store, which Ptah does not read",
		"app/seq: app.seq is a sequence, which Ptah does not read",
	})
	c.Assert(read.result.Tables, qt.HasLen, 0)
}

// A host that does not take unread tables is told nothing of an object Ptah
// does not model, and the read still goes on.
func TestWalkLeavesOutWhatItDoesNotDescribeWithoutUnreadTables(t *testing.T) {
	c := testcheck.New(t)
	read := nestedTraversal(listings(c, map[string][]*Ydb_Scheme.Entry{
		"/local": {{Name: "olap", Type: Ydb_Scheme.Entry_COLUMN_STORE}, {Name: "key", Type: Ydb_Scheme.Entry_SECRET}},
	}))

	_, err := read.directory(c.Context(), "")
	c.Assert(err, qt.IsNil)
	c.Assert(read.unreadTables(c.Context(), nil), qt.IsNil)

	c.Assert(nativeNames(read.result), qt.DeepEquals, []string{schema.SecretProperties + " key"})
	c.Assert(read.result.UnreadTables, qt.HasLen, 0)
}

// A queue of the older persistent queue kind is recorded, not read: no
// statement Ptah writes creates one, and the scheme service lists it apart
// from a topic YQL creates. The read asks the topic service nothing about it,
// so a plan neither keeps nor drops it. Ported from the host's
// TestReader_PersistentQueueIsRecorded.
func TestWalkRecordsAPersistentQueue(t *testing.T) {
	c := testcheck.New(t)
	read := nestedTraversal(listings(c, map[string][]*Ydb_Scheme.Entry{
		"/local": {{Name: "legacy", Type: Ydb_Scheme.Entry_PERS_QUEUE_GROUP}},
	}))

	_, err := read.directory(c.Context(), "")

	c.Assert(err, qt.IsNil)
	c.Assert(read.result.NativeObjects, qt.HasLen, 0)
	c.Assert(unknownKnowledge(read.result), qt.DeepEquals, []string{
		schema.TopicProperties + " legacy UNINSPECTED: a persistent queue group, which Ptah does not read",
	})
}

// An entry of a kind the read does not know is refused by its path and its
// kind, with what to do about it, rather than read as the nearest kind it
// knows. Ported from the host's TestReader_FailurePath.
func TestWalkRefusesAnEntryOfAKindItDoesNotKnow_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		entry *Ydb_Scheme.Entry
		want  string
	}{
		{name: "a block store volume", entry: &Ydb_Scheme.Entry{Name: "vol", Type: Ydb_Scheme.Entry_BLOCK_STORE_VOLUME},
			want: `YDB object /local/vol is a BLOCK_STORE_VOLUME, which Ptah does not read; remove it from the database or read a schema that does not hold it`},
		{name: "an RTMR volume", entry: &Ydb_Scheme.Entry{Name: "rtmr", Type: Ydb_Scheme.Entry_RTMR_VOLUME},
			want: `YDB object /local/rtmr is a RTMR_VOLUME, which Ptah does not read; remove it from the database or read a schema that does not hold it`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := nestedTraversal(listings(c, map[string][]*Ydb_Scheme.Entry{"/local": {test.entry}}))

			directories, err := read.directory(c.Context(), "")

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(directories, qt.IsNil)
		})
	}
}

// A coordination node whose description holds a mode or a field the pinned
// protocol buffers do not model is recorded as unrepresentable, because read
// as a node the provider can represent it would be planned away, and the read
// goes on. Ported from the host's
// TestReader_DescribesCoordinationNodes_UnknownState.
func TestWalkRecordsACoordinationNodeItCannotRepresent(t *testing.T) {
	unknownSetting := &Ydb_Coordination.Config{}
	unknownSetting.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 9, protowire.VarintType), 1))
	for _, test := range []struct {
		name   string
		config *Ydb_Coordination.Config
		reason string
	}{
		{name: "a read mode", config: &Ydb_Coordination.Config{ReadConsistencyMode: Ydb_Coordination.ConsistencyMode(7)},
			reason: "YDB coordination node /local/locks: its read_consistency_mode is 7, which Ptah does not know"},
		{name: "an attach mode", config: &Ydb_Coordination.Config{AttachConsistencyMode: Ydb_Coordination.ConsistencyMode(3)},
			reason: "YDB coordination node /local/locks: its attach_consistency_mode is 3, which Ptah does not know"},
		{name: "a counters mode", config: &Ydb_Coordination.Config{RateLimiterCountersMode: Ydb_Coordination.RateLimiterCountersMode(9)},
			reason: "YDB coordination node /local/locks: its rate_limiter_counters_mode is 9, which Ptah does not know"},
		{name: "a setting", config: unknownSetting,
			reason: "YDB coordination node /local/locks: its description carries field 9, which Ptah does not read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := nestedTraversal(listings(c, map[string][]*Ydb_Scheme.Entry{
				"/local": {{Name: "key", Type: Ydb_Scheme.Entry_SECRET}, {Name: "locks", Type: Ydb_Scheme.Entry_COORDINATION_NODE}},
			}))
			read.source.coordination = &describedNodes{nodes: map[string]*Ydb_Coordination.DescribeNodeResult{
				"/local/locks": {Self: &Ydb_Scheme.Entry{Name: "locks", Type: Ydb_Scheme.Entry_COORDINATION_NODE}, Config: test.config},
			}}

			_, err := read.directory(c.Context(), "")

			c.Assert(err, qt.IsNil)
			c.Assert(nativeNames(read.result), qt.DeepEquals, []string{schema.SecretProperties + " key"})
			c.Assert(unknownKnowledge(read.result), qt.DeepEquals, []string{schema.CoordinationProperties + " locks UNREPRESENTABLE: " + test.reason})
		})
	}
}

// The directory of the dev realms is left out only at the root of the
// database: a directory of that name below the root is schema like any other.
// Ported from the host's TestReader_LeavesOutTheDevRealms.
func TestWalkReadsADevRealmsDirectoryBelowTheRoot(t *testing.T) {
	c := testcheck.New(t)
	read := nestedTraversal(listings(c, map[string][]*Ydb_Scheme.Entry{
		"/local":     {{Name: "ptah_dev", Type: Ydb_Scheme.Entry_DIRECTORY}, {Name: "app", Type: Ydb_Scheme.Entry_DIRECTORY}},
		"/local/app": {{Name: "ptah_dev", Type: Ydb_Scheme.Entry_DIRECTORY}},
	}))
	read.leaveOut = []string{"ptah_dev"}

	root, err := read.directory(c.Context(), "")
	c.Assert(err, qt.IsNil)
	below, err := read.directory(c.Context(), "app")
	c.Assert(err, qt.IsNil)

	c.Assert(root, qt.DeepEquals, []string{"app"})
	c.Assert(below, qt.DeepEquals, []string{"app/ptah_dev"})
	c.Assert(read.skippedRealm, qt.IsTrue)
}

// A streaming query the read cannot establish settings for, as on a line
// without the streaming queries capability, is recorded as uninspected and is
// no object the access read describes: the read asks for no entry of it, so a
// path the account may not see cannot fail it. Ported from the host's
// TestReader_StreamingQueryPermissions_FailurePath; the object the read does
// describe is held by TestAccessOfAnObjectItCannotSee_FailurePath.
func TestWalkAsksNoEntryOfAStreamingQueryItCannotRead(t *testing.T) {
	c := testcheck.New(t)
	read := nestedTraversal(listings(c, map[string][]*Ydb_Scheme.Entry{
		"/local": {{Name: "copy", Type: streamingQueryEntry}},
	}))
	read.result.Complete, read.result.Engine, read.result.CommonObjects = true, "ydb", &protocol.ObservedCommonObjects{}
	_, err := read.directory(c.Context(), "")
	c.Assert(err, qt.IsNil)
	var asked []string
	describe := func(_ context.Context, absolute string) (*Ydb_Scheme.Entry, error) {
		asked = append(asked, absolute)
		return &Ydb_Scheme.Entry{Name: "local", Type: Ydb_Scheme.Entry_DATABASE, Owner: "root"}, nil
	}

	err = Access(c.Context(), read.result, "/local", DatabaseScope, describe, func(context.Context) (nativeapi.Principals, error) { return nativeapi.Principals{}, nil })

	c.Assert(err, qt.IsNil)
	c.Assert(unknownKnowledge(read.result), qt.DeepEquals, []string{
		schema.StreamingQueryProperties + " copy UNINSPECTED: the streaming query capability was not established",
	})
	c.Assert(asked, qt.Not(qt.Contains), "/local/copy")
}

// A row table's attributes become its comment and those of the columns and
// the indexes it has. YDB keeps a column's or an index's attribute after the
// column or the index is dropped; such an attribute names nothing the table
// holds and is no comment, and an attribute under a key that is not Ptah's is
// no comment either. Ported from the host's TestReader_ReadsComments.
func TestTableCommentsBelongToWhatTheTableHolds(t *testing.T) {
	c := testcheck.New(t)
	table := &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "app", Kind: "table", Name: "t"}
	described := &description{
		table: &native.DescribeTableResult{Columns: []*native.ColumnMeta{{Name: "id"}}, Attributes: map[string]string{
			ydbcomment.OwnKey: "the table", ydbcomment.ColumnKeyPrefix + "id": "the key", ydbcomment.ColumnKeyPrefix + "gone": "a dropped column",
			ydbcomment.IndexKeyPrefix + "by_id": "the index", ydbcomment.IndexKeyPrefix + "old": "a dropped index", "owner": "team",
		}},
		indexes: []*native.TableIndexDescription{{Name: "by_id"}},
	}
	into := &protocol.ObservedCommonObjects{}

	Common{Comments: true}.tableComments(into, table, described)

	var comments []string
	for _, comment := range into.Comments {
		comments = append(comments, comment.GetObject().GetKind()+" "+comment.GetObject().GetName()+": "+comment.GetText())
	}
	c.Assert(comments, qt.DeepEquals, []string{"table t: the table", "column id: the key", "index by_id: the index"})
}

// A table the listing names as a row table and whose description says it is
// a column table is refused rather than read as either. Ported from the
// host's TestReader_FailurePath.
func TestReadTableRefusesARowTableThatDescribesItselfAsAColumnTable(t *testing.T) {
	c := testcheck.New(t)
	described := &native.DescribeTableResult{Self: &Ydb_Scheme.Entry{Name: "t", Type: Ydb_Scheme.Entry_COLUMN_TABLE},
		Columns: []*native.ColumnMeta{{Name: "id"}}, PrimaryKey: []string{"id"}}

	table, err := readTable("/local", "", "t", described)

	c.Assert(err, qt.ErrorMatches, `YDB listing and table description disagree`)
	c.Assert(table, qt.IsNil)
}

// A replication or a transfer the read cannot describe because the cluster
// does not serve the replication API, as local-ydb does not by default, is
// recorded with the reason, and so is one whose description the provider
// cannot model; any other failure is no reason to record it and fails the
// read. Ported from the host's TestReader_RecordsWhatItDoesNotDescribe.
func TestUnreadReplicationReason(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{name: "the replication API unavailable", err: fmt.Errorf("describe YDB async replication: %w", nativeapi.ErrReplicationServiceUnavailable),
			want: "The cluster does not serve the replication API (YDB_GRPC_SERVICES lacks replication), so the provider leaves the object unmanaged"},
		{name: "a description it cannot model", err: fmt.Errorf("%w: its consistency level is 9", nativeapi.ErrUnreadable),
			want: "The provider leaves the object unmanaged: " + nativeapi.ErrUnreadable.Error() + ": its consistency level is 9"},
		{name: "another failure", err: errors.New("describe YDB async replication: transport failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(unreadReplication(test.err), qt.Equals, test.want)
		})
	}
}
