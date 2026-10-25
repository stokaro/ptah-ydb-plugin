package nativeapi_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/coordinationsql"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
)

// heldNode is a node at /local/app/node that states a self-check period of
// two seconds and nothing else.
func heldNode() *Ydb_Coordination.DescribeNodeResult {
	return &Ydb_Coordination.DescribeNodeResult{Config: &Ydb_Coordination.Config{SelfCheckPeriodMillis: 2_000},
		Self: &Ydb_Scheme.Entry{Name: "node", Type: Ydb_Scheme.Entry_COORDINATION_NODE, Owner: "owner"}}
}

func recognized(c *qt.C, text string) coordinationsql.Query {
	c.Helper()
	query, ok, err := coordinationsql.Recognize(text)
	c.Assert(err, qt.IsNil)
	c.Assert(ok, qt.IsTrue)
	return query
}

// Each row runs one statement against a node that is there or not, and the
// configuration the node holds afterward: what the statement names, every
// setting it leaves out kept, or sent unset on a creation.
func TestRunCoordinationStatement_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		held  *Ydb_Coordination.DescribeNodeResult
		text  string
		after *Ydb_Coordination.Config
	}{
		{name: "a creation sends only what it states", text: "CREATE COORDINATION NODE `app/node` WITH (read_consistency_mode = 'strict')",
			after: &Ydb_Coordination.Config{ReadConsistencyMode: Ydb_Coordination.ConsistencyMode_CONSISTENCY_MODE_STRICT}},
		{name: "a change sends only what it names", held: heldNode(),
			text:  "ALTER COORDINATION NODE `app/node` SET (session_grace_period = Interval('PT3.5S'))",
			after: &Ydb_Coordination.Config{SessionGracePeriodMillis: 3_500}},
		{name: "a path under a prefix the query sets", held: heldNode(),
			text:  "PRAGMA TablePathPrefix(\"/local/app\");\nALTER COORDINATION NODE `node` SET (rate_limiter_counters_mode = 'detailed')",
			after: &Ydb_Coordination.Config{RateLimiterCountersMode: Ydb_Coordination.RateLimiterCountersMode_RATE_LIMITER_COUNTERS_MODE_DETAILED}},
		{name: "a removal", held: heldNode(), text: "DROP COORDINATION NODE `/local/app/node`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			transport := &coordinationTransport{state: tc.held}
			dispatched, err := executor(transport).RunStatement(c.Context(), "/local", "/local", recognized(c, tc.text), allow)
			c.Assert(err, qt.IsNil)
			c.Assert(dispatched, qt.IsTrue)
			c.Assert(transport.writes, qt.Equals, 1)
			c.Assert(proto.Equal(transport.state.GetConfig(), tc.after), qt.IsTrue, qt.Commentf("%v", transport.state.GetConfig()))
		})
	}
}

// A realm session reads a relative path inside its realm's directory.
func TestRunCoordinationStatementInARealm(t *testing.T) {
	c := qt.New(t)
	transport := &coordinationTransport{}
	dispatched, err := executor(transport).RunStatement(c.Context(), "/local", "/local/ptah_dev/r1",
		recognized(c, "CREATE COORDINATION NODE `node`"), allow)
	c.Assert(err, qt.IsNil)
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(transport.writes, qt.Equals, 1)
}

// Each row is a statement refused before anything is sent, with the words
// of the refusal.
func TestRunCoordinationStatement_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		held      *Ydb_Coordination.DescribeNodeResult
		foreign   bool
		root      string
		text      string
		want      string
		statement bool
	}{
		{root: "/local", name: "a creation of a node that exists", held: heldNode(), text: "CREATE COORDINATION NODE `app/node`",
			want: "create YDB coordination node /local/app/node: the node already exists"},
		{root: "/local", name: "a change of a node that is not there", text: "ALTER COORDINATION NODE `app/node` SET (read_consistency_mode = 'strict')",
			want: "change YDB coordination node /local/app/node: no coordination node is at this path"},
		{root: "/local", name: "a removal of a node that is not there", text: "DROP COORDINATION NODE `app/node`",
			want: "drop YDB coordination node /local/app/node: no coordination node is at this path"},
		{root: "/local", name: "a change the node would not run with", held: heldNode(), statement: true,
			text: "ALTER COORDINATION NODE `app/node` SET (session_grace_period = Interval('PT2.5S'))",
			want: `invalid coordination node statement: change YDB coordination node /local/app/node: session_grace_period PT2.5S: YDB runs a node with a grace period from the self-check period plus PT1S \(PT3S here\) to PT30S and moves a period outside that range into it`},
		{root: "/local", name: "an object of another kind at the path", foreign: true, statement: true, text: "DROP COORDINATION NODE `app/node`",
			want: "invalid coordination node statement: /local/app/node is a YDB object of another kind than a coordination node"},
		{name: "a node outside a realm", root: "/local/ptah_dev/r1", statement: true, text: "CREATE COORDINATION NODE `/local/app/node`",
			want: "invalid coordination node statement: coordination node /local/app/node is not in /local/ptah_dev/r1"},
		{root: "/local", name: "the host's lock node", statement: true, text: "DROP COORDINATION NODE `ptah_locks`",
			want: "invalid coordination node statement: coordination node ptah_locks at the database root holds Ptah's own locks.*"},
		{root: "/local", name: "a segment the server owns", statement: true, text: "CREATE COORDINATION NODE `app/.sys`",
			want: `invalid coordination node statement: coordination node app/.sys has the path segment ".sys".*`},
		{root: "/local", name: "a relative prefix", statement: true, text: "PRAGMA TablePathPrefix('relp'); CREATE COORDINATION NODE `n`",
			want: `invalid coordination node statement: path relp/n is not in database /local.*`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			transport := &coordinationTransport{state: tc.held, foreign: tc.foreign}
			dispatched, err := executor(transport).RunStatement(c.Context(), "/local", tc.root, recognized(c, tc.text), allow)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(err, qt.ErrorAs, new(*nativeapi.StatementRefusal))
			c.Assert(errors.Is(err, coordinationsql.ErrStatement), qt.Equals, tc.statement)
			c.Assert(dispatched, qt.IsFalse)
			c.Assert(transport.writes, qt.Equals, 0)
		})
	}
}

// A lost lock stops the statement before the write, and a write whose reply
// is lost is dispatched: its outcome is unknown.
func TestRunCoordinationStatementDispatch(t *testing.T) {
	c := qt.New(t)
	lost := errors.New("lock lost")
	transport := &coordinationTransport{}
	dispatched, err := executor(transport).RunStatement(c.Context(), "/local", "/local", recognized(c, "CREATE COORDINATION NODE `app/node`"),
		func(context.Context) error { return lost })
	c.Assert(err, qt.ErrorIs, lost)
	c.Assert(dispatched, qt.IsFalse)
	c.Assert(transport.writes, qt.Equals, 0)

	transport = &coordinationTransport{failWrite: true}
	dispatched, err = executor(transport).RunStatement(c.Context(), "/local", "/local", recognized(c, "CREATE COORDINATION NODE `app/node`"), allow)
	c.Assert(err, qt.ErrorMatches, "YDB coordination node /local/app/node: transport failed")
	c.Assert(dispatched, qt.IsTrue)
	c.Assert(transport.writes, qt.Equals, 1)
}
