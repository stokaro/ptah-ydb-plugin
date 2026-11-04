package ydbfailure_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbfailure"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"
	"google.golang.org/protobuf/testing/protocmp"
)

// A live server answers the statuses below on demand only for some of them
// (integration/migration_split_live_test.go); these tests give each status as
// a decoded Answer.

func answer(status Ydb.StatusIds_StatusCode, issues ...string) ydbfailure.Answer {
	return ydbfailure.Answer{Status: status, Issues: issues}
}

// compiled is an answer in which YDB refused the text while compiling it.
func compiled(status Ydb.StatusIds_StatusCode, issues ...string) ydbfailure.Answer {
	return ydbfailure.Answer{Status: status, Issues: issues, Compiled: true}
}

// answered is an operation error as the SDK returns one: the status YDB
// answered with and the issues it attached.
type answered struct {
	status Ydb.StatusIds_StatusCode
	issues []*Ydb_Issue.IssueMessage
}

func (a *answered) Error() string                     { return "operation/" + a.status.String() }
func (a *answered) Code() int32                       { return int32(a.status) }
func (a *answered) Issues() []*Ydb_Issue.IssueMessage { return a.issues }

// issue is one issue YDB attaches, at row:column of the text (0:0 names no
// position), with the issues nested in it.
func issue(code uint32, row, column uint32, message string, nested ...*Ydb_Issue.IssueMessage) *Ydb_Issue.IssueMessage {
	return &Ydb_Issue.IssueMessage{IssueCode: code, Severity: 1, Message: message, Position: &Ydb_Issue.IssueMessage_Position{Row: row, Column: column}, Issues: nested}
}

// The answers are the ones 25.1.4.7 and 26.2.1.14 gave, measured, for a
// statement YDB refused while it compiled the text and for one it refused
// while it ran it. YDB names a position in the text only while it compiles,
// except inside its execution stage, which 25.1 words that way for a unique
// index it has no type for.
func TestReadPlacesARefusalBeforeExecution(t *testing.T) {
	for _, test := range []struct {
		name   string
		issues []*Ydb_Issue.IssueMessage
		want   ydbfailure.Answer
	}{
		{"an unknown type", []*Ydb_Issue.IssueMessage{issue(0, 1, 26, "Unknown simple type 'NoSuchType'")},
			compiled(Ydb.StatusIds_GENERIC_ERROR, "Unknown simple type 'NoSuchType'")},
		{"a parse error at the first column", []*Ydb_Issue.IssueMessage{issue(0, 1, 0, "mismatched input 'BROKEN' expecting {';'}")},
			compiled(Ydb.StatusIds_GENERIC_ERROR, "mismatched input 'BROKEN' expecting {';'}")},
		{"type annotation", []*Ydb_Issue.IssueMessage{issue(1030, 0, 0, "Type annotation",
			issue(0, 1, 33, "At function: KiAlterTable!", issue(0, 1, 33, "Column: \"id\" is a key column. Key column drop is not supported")))},
			compiled(Ydb.StatusIds_GENERIC_ERROR, "Type annotation", "At function: KiAlterTable!", "Column: \"id\" is a key column. Key column drop is not supported")},
		{"pre type annotation", []*Ydb_Issue.IssueMessage{issue(1020, 0, 0, "Pre type annotation", issue(0, 1, 9, "analyze is not supported for oltp tables."))},
			compiled(Ydb.StatusIds_GENERIC_ERROR, "Pre type annotation", "analyze is not supported for oltp tables.")},
		{"a position inside the execution stage", []*Ydb_Issue.IssueMessage{issue(1060, 0, 0, "Execution", issue(0, 1, 56, "Unknown index type: syncGlobalUnique"))},
			answer(Ydb.StatusIds_GENERIC_ERROR, "Execution", "Unknown index type: syncGlobalUnique")},
		{"a schema operation", []*Ydb_Issue.IssueMessage{issue(0, 0, 0, "Executing ESchemeOpCreatePersQueueGroup", issue(0, 0, 0, "Scheme operation failed, status: ExecError"))},
			answer(Ydb.StatusIds_GENERIC_ERROR, "Executing ESchemeOpCreatePersQueueGroup", "Scheme operation failed, status: ExecError")},
		// 26.2 words a schema operation's refusal this way, with no position;
		// the position nested in it is constructed, to pin that the stage
		// decides and not the position.
		{"a position inside a schema operation", []*Ydb_Issue.IssueMessage{issue(2017, 0, 0, "Executing ESchemeOpAlterTable", issue(2017, 1, 1, "Unsupported column type"))},
			answer(Ydb.StatusIds_GENERIC_ERROR, "Executing ESchemeOpAlterTable", "Unsupported column type")},
		{"no position", []*Ydb_Issue.IssueMessage{issue(0, 0, 0, "Scheme operation failed, status: ExecError")},
			answer(Ydb.StatusIds_GENERIC_ERROR, "Scheme operation failed, status: ExecError")},
		{"no issues", nil, answer(Ydb.StatusIds_GENERIC_ERROR)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			err := fmt.Errorf("execute: %w", &answered{status: Ydb.StatusIds_GENERIC_ERROR, issues: test.issues})
			c.Assert(ydbfailure.Read(err), qt.DeepEquals, test.want)
		})
	}
}

func TestReadKnowsNoStatusOfAnErrorYDBDidNotReturn(t *testing.T) {
	c := testcheck.New(t)
	for _, err := range []error{nil, errors.New("PRECONDITION_FAILED"), context.Canceled} {
		c.Assert(ydbfailure.Read(err), qt.DeepEquals, ydbfailure.Answer{})
	}
}

func TestConfirmed(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer ydbfailure.Answer
		text   string
		want   bool
	}{
		{"bad request", answer(Ydb.StatusIds_BAD_REQUEST), "CREATE TABLE t", true},
		{"scheme error", answer(Ydb.StatusIds_SCHEME_ERROR), "DROP TABLE t", true},
		{"precondition", answer(Ydb.StatusIds_PRECONDITION_FAILED), "INSERT INTO t (id) VALUES (1)", true},
		{"already exists", answer(Ydb.StatusIds_ALREADY_EXISTS), "CREATE TABLE t", true},
		{"not found", answer(Ydb.StatusIds_NOT_FOUND), "DROP TABLE t", true},
		{"unsupported", answer(Ydb.StatusIds_UNSUPPORTED), "ALTER TABLE t", true},
		{"aborted", answer(Ydb.StatusIds_ABORTED), "UPSERT INTO t (id) VALUES (1)", true},
		{"generic data", answer(Ydb.StatusIds_GENERIC_ERROR), "UPSERT INTO t (id) VALUES (\"x\")", true},
		{"generic scheme", answer(Ydb.StatusIds_GENERIC_ERROR), "CREATE TABLE t (id Int64, PRIMARY KEY (id))", false},
		{"generic scheme refused while compiling", compiled(Ydb.StatusIds_GENERIC_ERROR), "CREATE TABLE t (id NoSuchType, PRIMARY KEY (id))", true},
		{"generic block refused while compiling", compiled(Ydb.StatusIds_GENERIC_ERROR), "SELECT 1; DO BEGIN SELECT 1; END DO", true},
		{"generic batch", answer(Ydb.StatusIds_GENERIC_ERROR), "BATCH UPDATE t SET v = 1", false},
		{"generic block", answer(Ydb.StatusIds_GENERIC_ERROR), "SELECT 1; DO BEGIN SELECT 1; END DO", false},
		{"unavailable", answer(Ydb.StatusIds_UNAVAILABLE), "UPSERT INTO t (id) VALUES (1)", false},
		{"undetermined", answer(Ydb.StatusIds_UNDETERMINED), "UPSERT INTO t (id) VALUES (1)", false},
		{"timeout", answer(Ydb.StatusIds_TIMEOUT), "UPSERT INTO t (id) VALUES (1)", false},
		{"no status", answer(Ydb.StatusIds_STATUS_CODE_UNSPECIFIED), "UPSERT INTO t (id) VALUES (1)", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(test.answer.Confirmed(test.text), qt.Equals, test.want)
		})
	}
}

func TestRejected(t *testing.T) {
	for _, test := range []struct {
		name          string
		answer        ydbfailure.Answer
		text          string
		inTransaction bool
		want          bool
	}{
		{"data query", answer(Ydb.StatusIds_PRECONDITION_FAILED), "$v = 1u; INSERT INTO t (id) VALUES ($v); UPSERT INTO t (id) VALUES (2u)", false, true},
		{"scheme statement", answer(Ydb.StatusIds_SCHEME_ERROR), "DROP TABLE t", false, true},
		{"data block", answer(Ydb.StatusIds_PRECONDITION_FAILED), "DO BEGIN INSERT INTO t (id) VALUES (1); END DO", false, true},
		{"scheme block", answer(Ydb.StatusIds_ALREADY_EXISTS), "DO BEGIN CREATE TABLE t (id Int64, PRIMARY KEY (id)); END DO", false, false},
		{"scheme statement refused while compiling", compiled(Ydb.StatusIds_GENERIC_ERROR), "$id = 1l; CREATE TABLE t (id NoSuchType NOT NULL, PRIMARY KEY (id))", false, true},
		{"scheme block refused while compiling", compiled(Ydb.StatusIds_GENERIC_ERROR), "DO BEGIN CREATE TABLE t (id NoSuchType, PRIMARY KEY (id)); END DO", false, false},
		{"scheme statement refused while running", answer(Ydb.StatusIds_GENERIC_ERROR), "CREATE TOPIC t", false, false},
		{"scheme loop", answer(Ydb.StatusIds_ALREADY_EXISTS), "DEFINE ACTION $m($n) AS CREATE TABLE $n (id Int64, PRIMARY KEY (id)); END DEFINE; EVALUATE FOR $t IN AsList('a', 'b') DO $m($t)", false, false},
		{"batch", answer(Ydb.StatusIds_PRECONDITION_FAILED), "BATCH DELETE FROM t WHERE id > 0", false, false},
		{"two scheme statements", answer(Ydb.StatusIds_ALREADY_EXISTS), "CREATE TABLE a (id Int64, PRIMARY KEY (id)); CREATE TABLE b (id Int64, PRIMARY KEY (id))", false, false},
		{"unsplittable", answer(Ydb.StatusIds_BAD_REQUEST), "--!ansi_lexer\nSELECT 1", false, false},
		{"batch in a transaction", answer(Ydb.StatusIds_PRECONDITION_FAILED), "BATCH DELETE FROM t WHERE id > 0", true, true},
		{"scheme block in a transaction", answer(Ydb.StatusIds_PRECONDITION_FAILED), "DO BEGIN CREATE TABLE t (id Int64, PRIMARY KEY (id)); END DO", true, true},
		{"unconfirmed in a transaction", answer(Ydb.StatusIds_UNAVAILABLE), "UPSERT INTO t (id) VALUES (1)", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(test.answer.Rejected(test.text, test.inTransaction), qt.Equals, test.want)
		})
	}
}

func TestMessageIsTheServersIssuesOnOneLine(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer ydbfailure.Answer
		want   string
	}{
		{"issues", answer(Ydb.StatusIds_PRECONDITION_FAILED, "Constraint violated. Table: `/local/t`.", "Conflict with existing key."), "Constraint violated. Table: `/local/t`.; Conflict with existing key."},
		{"line breaks", answer(Ydb.StatusIds_GENERIC_ERROR, "first\nsecond\r\n", "\tthird"), "first second; third"},
		{"none", answer(Ydb.StatusIds_ABORTED), "fallback"},
		{"long", answer(Ydb.StatusIds_BAD_REQUEST, strings.Repeat("é", 300)), strings.Repeat("é", 256)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(test.answer.Message("fallback"), qt.Equals, test.want)
		})
	}
}

func TestFailed(t *testing.T) {
	const insert = "INSERT INTO t (id) VALUES (1u)"
	conflict := answer(Ydb.StatusIds_ABORTED, "Transaction locks invalidated. Table: `/local/t`.")
	duplicate := answer(Ydb.StatusIds_PRECONDITION_FAILED, "Conflict with existing key.")
	unknown := &protocol.Execution{Outcome: protocol.Execution_UNKNOWN, DiagnosticCode: "ydb.execution-unconfirmed",
		Message: "The dispatched unit's outcome is unknown; no automatic retry was attempted"}
	unknownAfterOne := &protocol.Execution{Outcome: protocol.Execution_UNKNOWN, AppliedCount: 1, DiagnosticCode: unknown.DiagnosticCode, Message: unknown.Message}
	for _, test := range []struct {
		name    string
		answer  ydbfailure.Answer
		unit    ydbfailure.Unit
		applied uint32
		want    *protocol.Execution
	}{
		{"rejected", duplicate, ydbfailure.Unit{Text: insert}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "Conflict with existing key."}},
		{"rejected after a prefix", duplicate, ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 2,
			&protocol.Execution{Outcome: protocol.Execution_PARTIAL, AppliedCount: 2, DiagnosticCode: "ydb.statement-rejected", Message: "Conflict with existing key."}},
		{"rejected while compiling", compiled(Ydb.StatusIds_GENERIC_ERROR, "Unknown simple type 'NoSuchType'"), ydbfailure.Unit{Text: "CREATE TABLE t (id NoSuchType NOT NULL, PRIMARY KEY (id))"}, 2,
			&protocol.Execution{Outcome: protocol.Execution_PARTIAL, AppliedCount: 2, DiagnosticCode: "ydb.statement-rejected", Message: "Unknown simple type 'NoSuchType'"}},
		{"refused while running", answer(Ydb.StatusIds_GENERIC_ERROR, "Scheme operation failed"), ydbfailure.Unit{Text: "CREATE TOPIC t"}, 0, unknown},
		{"rejected without issues", answer(Ydb.StatusIds_SCHEME_ERROR), ydbfailure.Unit{Text: "DROP TABLE t"}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "YDB rejected the unit"}},
		{"aborted", conflict, ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.transaction-aborted", Message: "Transaction locks invalidated. Table: `/local/t`.", TransactionAborted: true}},
		{"aborted, not negotiated", conflict, ydbfailure.Unit{Text: insert, Transaction: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.transaction-aborted", Message: "Transaction locks invalidated. Table: `/local/t`."}},
		{"aborted after a prefix", conflict, ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 1,
			&protocol.Execution{Outcome: protocol.Execution_PARTIAL, AppliedCount: 1, DiagnosticCode: "ydb.transaction-aborted", Message: "Transaction locks invalidated. Table: `/local/t`."}},
		{"aborted without issues", answer(Ydb.StatusIds_ABORTED), ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.transaction-aborted", Message: "YDB aborted the transaction for a conflict", TransactionAborted: true}},
		{"aborted outside a transaction", conflict, ydbfailure.Unit{Text: insert, Aborts: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "Transaction locks invalidated. Table: `/local/t`."}},
		{"native", duplicate, ydbfailure.Unit{}, 0, unknown},
		{"service statement in a transaction", duplicate, ydbfailure.Unit{Transaction: true, Aborts: true}, 0, unknown},
		{"not applied whole", duplicate, ydbfailure.Unit{Text: "BATCH DELETE FROM t WHERE id > 0"}, 0, unknown},
		{"unconfirmed", answer(Ydb.StatusIds_UNDETERMINED), ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 0, unknown},
		{"unconfirmed after a prefix", answer(Ydb.StatusIds_TIMEOUT), ydbfailure.Unit{Text: insert}, 1, unknownAfterOne},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbfailure.Failed(test.answer, test.unit, test.applied), qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

func TestCommitOfAnAbortedTransactionIsRolledBack(t *testing.T) {
	for _, test := range []struct {
		name   string
		aborts bool
		want   *protocol.TransactionResult
	}{
		{"negotiated", true, &protocol.TransactionResult{Outcome: protocol.TransactionResult_ROLLED_BACK, Aborted: true, DiagnosticCode: "ydb.transaction-aborted", Message: "Transaction locks invalidated."}},
		{"not negotiated", false, &protocol.TransactionResult{Outcome: protocol.TransactionResult_ROLLED_BACK, DiagnosticCode: "ydb.transaction-aborted", Message: "Transaction locks invalidated."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbfailure.Commit(answer(Ydb.StatusIds_ABORTED, "Transaction locks invalidated."), test.aborts), qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

func TestCommitOfAnotherFailureIsUnknown(t *testing.T) {
	c := testcheck.New(t)
	for _, status := range []Ydb.StatusIds_StatusCode{Ydb.StatusIds_STATUS_CODE_UNSPECIFIED, Ydb.StatusIds_UNDETERMINED, Ydb.StatusIds_PRECONDITION_FAILED, Ydb.StatusIds_UNAVAILABLE} {
		c.Assert(ydbfailure.Commit(answer(status), true), qt.IsNil, qt.Commentf("%s", status))
	}
}

// TestPathBusy reports the refusal YDB gives a schema statement on a path
// another schema operation holds, in either of the words it gives one, and no
// other answer.
func TestPathBusy(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer ydbfailure.Answer
		want   bool
	}{
		{"a path an initial scan holds", answer(Ydb.StatusIds_OVERLOADED, "Check failed: path: '/local/d/events', error: path has been locked by tx: 281474976710672"), true},
		{"a path under operation", answer(Ydb.StatusIds_OVERLOADED, "Check failed: path: '/local/d/events', error: path is under operation"), true},
		{"another overload", answer(Ydb.StatusIds_OVERLOADED, "too many requests"), false},
		{"an overload with no words", answer(Ydb.StatusIds_OVERLOADED), false},
		{"the words with another status", answer(Ydb.StatusIds_SCHEME_ERROR, "path is under operation"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(test.answer.PathBusy(), qt.Equals, test.want)
		})
	}
}
