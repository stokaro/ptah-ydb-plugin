package repeatables

// White-box testing required: which row a claim, a completion or a failure
// writes, and which statement writes it, is reachable through the exported
// Mutate only over a YDB connection. This test pins each transition without
// one; the live tests in integration/ prove the same transitions, and their
// atomicity, on the engine.

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func running() *protocol.RepeatableRow {
	return &protocol.RepeatableRow{Identity: "names", State: protocol.RepeatableRow_RUNNING, Checksum: "old", AttemptChecksum: "new",
		AttemptOrder: 2, RunId: "run-1", AttemptedAtSeconds: 10, Applied: true, AppliedAtSeconds: 5, BodyKept: true}
}

func claim(insert bool, expected string) *protocol.MutateRepeatableRequest {
	return &protocol.MutateRepeatableRequest{Mutation: &protocol.MutateRepeatableRequest_Claim{Claim: &protocol.RepeatableClaim{
		Identity: "names", Insert: insert, ExpectedRunId: expected, RunId: "run-2", AttemptChecksum: "next", AttemptOrder: 3,
		Transactional: true, AttemptedAtSeconds: 20, AttemptedAtNanos: 7}}}
}

func TestNextRow_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		request *protocol.MutateRepeatableRequest
		current *protocol.RepeatableRow
		want    *protocol.RepeatableRow
		write   write
	}{
		{
			name:    "an inserting claim on no row",
			request: claim(true, ""),
			want: &protocol.RepeatableRow{Identity: "names", State: protocol.RepeatableRow_RUNNING, AttemptChecksum: "next", AttemptOrder: 3,
				Transactional: true, RunId: "run-2", AttemptedAtSeconds: 20, AttemptedAtNanos: 7},
			write: insertRow,
		},
		{
			name:    "a claim that takes over the run it read keeps what applied",
			request: claim(false, "run-1"),
			current: failedRunning(),
			want: &protocol.RepeatableRow{Identity: "names", State: protocol.RepeatableRow_RUNNING, Checksum: "old", AttemptChecksum: "next",
				AttemptOrder: 3, Transactional: true, RunId: "run-2", AttemptedAtSeconds: 20, AttemptedAtNanos: 7, Applied: true,
				AppliedAtSeconds: 5, BodyKept: true},
			write: updateRow,
		},
		{
			name: "a completion of the claimed digest",
			request: &protocol.MutateRepeatableRequest{Mutation: &protocol.MutateRepeatableRequest_Complete{Complete: &protocol.RepeatableCompletion{
				Identity: "names", RunId: "run-1", Checksum: "new", Body: make([]byte, 0), AppliedAtSeconds: 30, AppliedAtNanos: 9}}},
			current: running(),
			want: &protocol.RepeatableRow{Identity: "names", State: protocol.RepeatableRow_APPLIED, Checksum: "new", AttemptChecksum: "new",
				AttemptOrder: 2, RunId: "run-1", AttemptedAtSeconds: 10, Applied: true, AppliedAtSeconds: 30, AppliedAtNanos: 9, BodyKept: true},
			write: completeRow,
		},
		{
			name: "a failure of the claiming run",
			request: &protocol.MutateRepeatableRequest{Mutation: &protocol.MutateRepeatableRequest_Fail{Fail: &protocol.RepeatableFailure{
				Identity: "names", RunId: "run-1", Error: "boom"}}},
			current: running(),
			want: &protocol.RepeatableRow{Identity: "names", State: protocol.RepeatableRow_FAILED, Checksum: "old", AttemptChecksum: "new",
				AttemptOrder: 2, RunId: "run-1", AttemptedAtSeconds: 10, Applied: true, AppliedAtSeconds: 5, Error: "boom", BodyKept: true},
			write: updateRow,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			before := proto.CloneOf(test.current)
			next, statement, refusal := nextRow(test.request, test.current)
			c.Assert(refusal, qt.IsNil)
			c.Assert(proto.Equal(next, test.want), qt.IsTrue, qt.Commentf("%v", next))
			c.Assert(statement, qt.Equals, test.write)
			c.Assert(proto.Equal(test.current, before), qt.IsTrue)
		})
	}
}

func TestNextRow_FailurePath(t *testing.T) {
	complete := func(run, checksum string) *protocol.MutateRepeatableRequest {
		return &protocol.MutateRepeatableRequest{Mutation: &protocol.MutateRepeatableRequest_Complete{Complete: &protocol.RepeatableCompletion{
			Identity: "names", RunId: run, Checksum: checksum, AppliedAtSeconds: 30}}}
	}
	fail := &protocol.MutateRepeatableRequest{Mutation: &protocol.MutateRepeatableRequest_Fail{Fail: &protocol.RepeatableFailure{
		Identity: "names", RunId: "run-2", Error: "boom"}}}
	for _, test := range []struct {
		name    string
		request *protocol.MutateRepeatableRequest
		current *protocol.RepeatableRow
		want    Result
	}{
		{"an inserting claim meets a row", claim(true, ""), running(), Result{Outcome: protocol.RepeatableResult_CONFLICT, Holder: "run-1"}},
		{"a claim expects another run", claim(false, "run-0"), running(), Result{Outcome: protocol.RepeatableResult_CONFLICT, Holder: "run-1"}},
		{"a claim expects a row that is gone", claim(false, "run-1"), nil, Result{Outcome: protocol.RepeatableResult_CONFLICT}},
		{"a completion of another run", complete("run-2", "new"), running(), Result{Outcome: protocol.RepeatableResult_CONFLICT, Holder: "run-1"}},
		{"a completion of a row that is gone", complete("run-1", "new"), nil, Result{Outcome: protocol.RepeatableResult_CONFLICT}},
		{"a completion of another digest", complete("run-1", "other"), running(), Result{Outcome: protocol.RepeatableResult_REFUSED}},
		{"a failure of another run", fail, running(), Result{Outcome: protocol.RepeatableResult_CONFLICT, Holder: "run-1"}},
		{"a failure of a row that is gone", fail, nil, Result{Outcome: protocol.RepeatableResult_CONFLICT}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			next, _, refusal := nextRow(test.request, test.current)
			c.Assert(next, qt.IsNil)
			c.Assert(refusal, qt.IsNotNil)
			c.Assert(*refusal, qt.Equals, test.want)
		})
	}
}

// failedRunning is the running row of run-1 after its attempt failed.
func failedRunning() *protocol.RepeatableRow {
	row := running()
	row.State, row.Error = protocol.RepeatableRow_FAILED, "boom"
	return row
}
