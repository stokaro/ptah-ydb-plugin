package nativeapi_test

// These tests run a creation whose directory does not exist through each
// native operation's read before the write. YDB creates the directories an
// object needs, so the read has to find no occupant there, and refuse a
// directory the database lists but does not show to the session.

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/grpc"

	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// errLostReply stands for a write whose reply never arrived: the write was
// dispatched, so the read before it established what the creation needs.
var errLostReply = errors.New("lost reply")

// statements records the statements a creation sends and loses each reply.
type statements struct{ sent []string }

func (s *statements) execute(_ context.Context, statement string) error {
	s.sent = append(s.sent, statement)
	return errLostReply
}

// nodeWrites is a coordination service that records the methods it is asked
// to run and loses each reply.
type nodeWrites struct{ methods []string }

func (n *nodeWrites) Invoke(_ context.Context, method string, _, _ any, _ ...grpc.CallOption) error {
	n.methods = append(n.methods, method)
	return errLostReply
}

func (n *nodeWrites) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("unexpected stream")
}

// tableWrites is a native table session that records the paths it is asked
// to create and loses each reply.
type tableWrites struct{ created []string }

func (s *tableWrites) Create(_ context.Context, request *Ydb_Table.CreateTableRequest) error {
	s.created = append(s.created, request.Path)
	return errLostReply
}
func (s *tableWrites) Describe(context.Context, string) (*native.DescribeTableResult, error) {
	return nil, errors.New("unexpected description")
}
func (s *tableWrites) DescribeColumnTable(context.Context, string) (*native.ColumnTableDescribeResult, error) {
	return nil, errors.New("unexpected description")
}
func (s *tableWrites) Close() error { return nil }

func topicCreation(c *qt.C, directory string) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Database: "/local", Schema: directory, Name: "events"}
	intent, err := topics.Intent(ydbdecl.Topic{})
	c.Assert(err, qt.IsNil)
	desired, err := schema.CaptureTopicIntent(owner, intent)
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureTopicChange("change", owner, nil, desired)
	c.Assert(err, qt.IsNil)
	operation, err := schema.TopicOperationFromChange("operation", change.Value)
	c.Assert(err, qt.IsNil)
	return operation.Value
}

func coordinationCreation(c *qt.C, directory string) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Database: "/local", Schema: directory, Name: "node"}
	desired, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{ReadConsistencyMode: new("strict")})
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureCoordinationChange("change", owner, nil, desired)
	c.Assert(err, qt.IsNil)
	operation, err := schema.CoordinationOperationFromChange("operation", change.Value)
	c.Assert(err, qt.IsNil)
	return operation.Value
}

func secretCreation(c *qt.C, directory string) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: directory, Name: "pg.pw"}
	desired, err := schema.CaptureSecretIntent(owner, &native.SecretIntent{ValueEnv: "PTAH_SECRET_PG"})
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureSecretChange("change", owner, nil, desired)
	c.Assert(err, qt.IsNil)
	operation, err := schema.SecretOperationFromChange("operation", change.Value)
	c.Assert(err, qt.IsNil)
	return operation.Value
}

func externalSourceCreation(c *qt.C, directory string) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.ExternalSourceProperties, Database: "/local", Schema: directory, Name: "bucket"}
	intent, err := externals.SourceIntent(ydbdecl.ExternalDataSource{SourceType: "ObjectStorage", Location: "https://storage.example.test/events/", AuthMethod: "NONE"})
	c.Assert(err, qt.IsNil)
	desired, err := schema.CaptureExternalIntent(owner, intent)
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureExternalChange(schema.ExternalSourceProperties, "change", owner, "/local", nil, desired)
	c.Assert(err, qt.IsNil)
	operation, err := schema.ExternalOperation("operation", change.Value, schema.ExternalCreate)
	c.Assert(err, qt.IsNil)
	return operation.Value
}

func rowTableCreation(c *qt.C, directory string) *protocol.Envelope {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: directory, Name: "records"}
	operation, err := tablecreate.Capture("create", owner, &protocol.DeclaredTable{Name: "records", Schema: directory, PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}}}, nil)
	c.Assert(err, qt.IsNil)
	return operation.Value
}

func columnTableCreation(c *qt.C, directory string) *protocol.Envelope {
	c.Helper()
	return tableOperation(c, &protocol.DeclaredTable{Name: "c1", Schema: directory, PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}}}, `{"hashColumns":["id"]}`).Value
}

func TestTopicCreationInANewDirectory_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	sent := &statements{}
	topic := &nativeapi.Topic{Scheme: newDirectory(), Execute: sent.execute}
	dispatched, err := topic.Apply(c.Context(), "/local", topicCreation(c, "fresh/nested"), allow)
	c.Assert(err, qt.ErrorIs, errLostReply)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(sent.sent, qt.HasLen, 1)
	c.Assert(sent.sent[0], qt.Matches, "CREATE TOPIC `/local/fresh/nested/events`.*")
}

func TestTopicCreationInANewDirectory_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	sent := &statements{}
	topic := &nativeapi.Topic{Scheme: newDirectory(), Execute: sent.execute}
	dispatched, err := topic.Apply(c.Context(), "/local", topicCreation(c, "hidden/nested"), allow)
	c.Assert(err, qt.ErrorIs, nativeapi.ErrDirectoryNotVisible)
	c.Assert(err, qt.ErrorAs, new(*nativeapi.Refusal))
	c.Assert(err, qt.ErrorMatches, "read topic hidden/nested/events: list YDB directory /local/hidden: its parent lists it, .*")
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(sent.sent, qt.HasLen, 0)
}

func TestCoordinationCreationInANewDirectory_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	writes := &nodeWrites{}
	nodes := &nativeapi.Coordination{Scheme: newDirectory(), Nodes: Ydb_Coordination_V1.NewCoordinationServiceClient(writes)}
	dispatched, err := nodes.Apply(c.Context(), "/local", coordinationCreation(c, "fresh/nested"), allow)
	c.Assert(err, qt.IsNotNil)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(writes.methods, qt.DeepEquals, []string{"/Ydb.Coordination.V1.CoordinationService/CreateNode"})
}

func TestCoordinationCreationInANewDirectory_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	writes := &nodeWrites{}
	nodes := &nativeapi.Coordination{Scheme: newDirectory(), Nodes: Ydb_Coordination_V1.NewCoordinationServiceClient(writes)}
	dispatched, err := nodes.Apply(c.Context(), "/local", coordinationCreation(c, "hidden/nested"), allow)
	c.Assert(err, qt.ErrorIs, nativeapi.ErrDirectoryNotVisible)
	c.Assert(err, qt.ErrorAs, new(*nativeapi.Refusal))
	c.Assert(err, qt.ErrorMatches, "read coordination node hidden/nested/node: list YDB directory /local/hidden: its parent lists it, .*")
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(writes.methods, qt.HasLen, 0)
}

func TestCoordinationStatementInANewDirectory_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	writes := &nodeWrites{}
	nodes := &nativeapi.Coordination{Scheme: newDirectory(), Nodes: Ydb_Coordination_V1.NewCoordinationServiceClient(writes)}
	dispatched, err := nodes.RunStatement(c.Context(), "/local", "/local", recognized(c, "CREATE COORDINATION NODE `fresh/nested/node`"), allow)
	c.Assert(err, qt.IsNotNil)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(writes.methods, qt.DeepEquals, []string{"/Ydb.Coordination.V1.CoordinationService/CreateNode"})
}

func TestCoordinationStatementInANewDirectory_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	writes := &nodeWrites{}
	nodes := &nativeapi.Coordination{Scheme: newDirectory(), Nodes: Ydb_Coordination_V1.NewCoordinationServiceClient(writes)}
	dispatched, err := nodes.RunStatement(c.Context(), "/local", "/local", recognized(c, "CREATE COORDINATION NODE `hidden/nested/node`"), allow)
	c.Assert(err, qt.ErrorIs, nativeapi.ErrDirectoryNotVisible)
	c.Assert(err, qt.ErrorAs, new(*nativeapi.Refusal))
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(writes.methods, qt.HasLen, 0)
}

func TestSecretCreationInANewDirectory_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	sent := &statements{}
	secret := &nativeapi.Secret{Scheme: newDirectory(), Execute: sent.execute}
	dispatched, err := secret.Apply(c.Context(), "/local", secretCreation(c, "fresh/nested"), allow)
	c.Assert(err, qt.ErrorIs, errLostReply)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(sent.sent, qt.DeepEquals, []string{"CREATE SECRET `/local/fresh/nested/pg.pw` WITH (value = $PTAH_SECRET_PG);"})
}

func TestSecretCreationInANewDirectory_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	sent := &statements{}
	secret := &nativeapi.Secret{Scheme: newDirectory(), Execute: sent.execute}
	dispatched, err := secret.Apply(c.Context(), "/local", secretCreation(c, "hidden/nested"), allow)
	c.Assert(err, qt.ErrorIs, nativeapi.ErrDirectoryNotVisible)
	c.Assert(err, qt.ErrorAs, new(*nativeapi.Refusal))
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(sent.sent, qt.HasLen, 0)
}

func TestExternalCreationInANewDirectory_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	sent := &statements{}
	external := &nativeapi.External{Scheme: newDirectory(), Execute: sent.execute}
	dispatched, err := external.Apply(c.Context(), "/local", externalSourceCreation(c, "fresh/nested"), allow)
	c.Assert(err, qt.ErrorIs, errLostReply)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(sent.sent, qt.HasLen, 1)
	c.Assert(sent.sent[0], qt.Matches, "(?s)CREATE EXTERNAL DATA SOURCE `/local/fresh/nested/bucket`.*")
}

func TestExternalCreationInANewDirectory_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	sent := &statements{}
	external := &nativeapi.External{Scheme: newDirectory(), Execute: sent.execute}
	dispatched, err := external.Apply(c.Context(), "/local", externalSourceCreation(c, "hidden/nested"), allow)
	c.Assert(err, qt.ErrorIs, nativeapi.ErrDirectoryNotVisible)
	c.Assert(err, qt.ErrorAs, new(*nativeapi.Refusal))
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(sent.sent, qt.HasLen, 0)
}

func TestRowTableCreationInANewDirectory_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	session := &tableWrites{}
	creation := &nativeapi.TableCreation{Scheme: newDirectory(), Open: func(context.Context) (nativeapi.CreationSession, error) { return session, nil }}
	dispatched, err := creation.Apply(c.Context(), "/local", rowTableCreation(c, "fresh/nested"), allow)
	c.Assert(err, qt.ErrorIs, errLostReply)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(session.created, qt.DeepEquals, []string{"/local/fresh/nested/records"})
}

func TestRowTableCreationInANewDirectory_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, directory, want string }{
		{name: "a directory the session cannot list", directory: "hidden/nested",
			want: "read table hidden/nested/records: list YDB directory /local/hidden: its parent lists it, .*"},
		{name: "a directory below a table", directory: "records/nested",
			want: "read table records/nested/records: list YDB directory /local/records/nested: its parent is not a directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			session := &tableWrites{}
			creation := &nativeapi.TableCreation{Scheme: newDirectory(), Open: func(context.Context) (nativeapi.CreationSession, error) { return session, nil }}
			dispatched, err := creation.Apply(c.Context(), "/local", rowTableCreation(c, test.directory), allow)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(err, qt.ErrorAs, new(*nativeapi.Refusal))
			c.Assert(dispatched, qt.IsFalse)
			c.Assert(session.created, qt.HasLen, 0)
		})
	}
}

func TestColumnTableCreationInANewDirectory_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	sent := &statements{}
	creation := &nativeapi.ColumnTableCreation{Scheme: newDirectory(), Execute: sent.execute,
		Open:       func(context.Context) (nativeapi.ColumnDescriber, error) { return &tableWrites{}, nil },
		Monitoring: func(context.Context, string) ([]byte, error) { return nil, errors.New("unexpected monitoring read") }}
	dispatched, err := creation.Apply(c.Context(), "/local", columnTableCreation(c, "fresh/nested"), allow)
	c.Assert(err, qt.ErrorIs, errLostReply)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(sent.sent, qt.HasLen, 1)
	c.Assert(sent.sent[0], qt.Matches, "(?s)CREATE TABLE `/local/fresh/nested/c1`.*")
}

func TestColumnTableCreationInANewDirectory_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	sent := &statements{}
	creation := &nativeapi.ColumnTableCreation{Scheme: newDirectory(), Execute: sent.execute,
		Open:       func(context.Context) (nativeapi.ColumnDescriber, error) { return &tableWrites{}, nil },
		Monitoring: func(context.Context, string) ([]byte, error) { return nil, errors.New("unexpected monitoring read") }}
	dispatched, err := creation.Apply(c.Context(), "/local", columnTableCreation(c, "hidden/nested"), allow)
	c.Assert(err, qt.ErrorIs, nativeapi.ErrDirectoryNotVisible)
	c.Assert(err, qt.ErrorAs, new(*nativeapi.Refusal))
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(sent.sent, qt.HasLen, 0)
}
