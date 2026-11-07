package ydbfailure_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbfailure"
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

// located is answer with the most specific issue code and position YDB gave.
func located(answer ydbfailure.Answer, code, row, column uint32) ydbfailure.Answer {
	answer.Code, answer.Row, answer.Column = code, row, column
	return answer
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
func issue(code, row, column uint32, message string, nested ...*Ydb_Issue.IssueMessage) *Ydb_Issue.IssueMessage {
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
			located(compiled(Ydb.StatusIds_GENERIC_ERROR, "Unknown simple type 'NoSuchType'"), 0, 1, 26)},
		{"a parse error at the first column", []*Ydb_Issue.IssueMessage{issue(0, 1, 0, "mismatched input 'BROKEN' expecting {';'}")},
			located(compiled(Ydb.StatusIds_GENERIC_ERROR, "mismatched input 'BROKEN' expecting {';'}"), 0, 1, 0)},
		{"type annotation", []*Ydb_Issue.IssueMessage{issue(1030, 0, 0, "Type annotation",
			issue(0, 1, 33, "At function: KiAlterTable!", issue(0, 1, 33, "Column: \"id\" is a key column. Key column drop is not supported")))},
			located(compiled(Ydb.StatusIds_GENERIC_ERROR, "Type annotation", "At function: KiAlterTable!", "Column: \"id\" is a key column. Key column drop is not supported"), 1030, 1, 33)},
		{"pre type annotation", []*Ydb_Issue.IssueMessage{issue(1020, 0, 0, "Pre type annotation", issue(0, 1, 9, "analyze is not supported for oltp tables."))},
			located(compiled(Ydb.StatusIds_GENERIC_ERROR, "Pre type annotation", "analyze is not supported for oltp tables."), 1020, 1, 9)},
		{"a position inside the execution stage", []*Ydb_Issue.IssueMessage{issue(1060, 0, 0, "Execution", issue(0, 1, 56, "Unknown index type: syncGlobalUnique"))},
			located(answer(Ydb.StatusIds_GENERIC_ERROR, "Execution", "Unknown index type: syncGlobalUnique"), 1060, 1, 56)},
		{"a schema operation", []*Ydb_Issue.IssueMessage{issue(0, 0, 0, "Executing ESchemeOpCreatePersQueueGroup", issue(0, 0, 0, "Scheme operation failed, status: ExecError"))},
			answer(Ydb.StatusIds_GENERIC_ERROR, "Executing ESchemeOpCreatePersQueueGroup", "Scheme operation failed, status: ExecError")},
		// 26.2 words a schema operation's refusal this way, with no position;
		// the position nested in it is constructed, to pin that the stage
		// decides and not the position.
		{"a position inside a schema operation", []*Ydb_Issue.IssueMessage{issue(2017, 0, 0, "Executing ESchemeOpAlterTable", issue(2017, 1, 1, "Unsupported column type"))},
			located(answer(Ydb.StatusIds_GENERIC_ERROR, "Executing ESchemeOpAlterTable", "Unsupported column type"), 2017, 1, 1)},
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

// sizeAbort is the answer 25.1.4.7 gives the commit of a transaction whose
// writes do not fit one datashard program, measured: ABORTED, as a conflict is
// answered, with issue 200509.
var sizeAbort = ydbfailure.Answer{Status: Ydb.StatusIds_ABORTED, Issues: []string{"Datashard program size limit exceeded (56281509 > 50331648)"},
	Code: 200509, Repeats: true}

// An ABORTED answer whose issue says the transaction does not fit one
// datashard program repeats on every attempt, nested or not, so it is not an
// abort for a conflict; one for a lock conflict is.
func TestReadNamesAnAbortThatRepeats(t *testing.T) {
	for _, test := range []struct {
		name    string
		issues  []*Ydb_Issue.IssueMessage
		repeats bool
		aborted bool
	}{
		{name: "too large for one program", issues: []*Ydb_Issue.IssueMessage{issue(200509, 0, 0, "Datashard program size limit exceeded (56281509 > 50331648)")},
			repeats: true},
		{name: "too large, nested", issues: []*Ydb_Issue.IssueMessage{issue(1060, 0, 0, "Execution", issue(200509, 0, 0, "Datashard program size limit exceeded (56281509 > 50331648)"))},
			repeats: true},
		{name: "a lock conflict", issues: []*Ydb_Issue.IssueMessage{issue(2001, 0, 0, "Transaction locks invalidated. Table: `/local/t`.")}, aborted: true},
		{name: "no issues", aborted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := ydbfailure.Read(fmt.Errorf("commit: %w", &answered{status: Ydb.StatusIds_ABORTED, issues: test.issues}))
			c.Assert(read.Repeats, qt.Equals, test.repeats)
			c.Assert(read.Aborted(), qt.Equals, test.aborted)
		})
	}
}

func TestReadKnowsNoStatusOfAnErrorYDBDidNotReturn(t *testing.T) {
	c := testcheck.New(t)
	for _, err := range []error{nil, errors.New("PRECONDITION_FAILED"), context.Canceled} {
		c.Assert(ydbfailure.Read(err), qt.DeepEquals, ydbfailure.Answer{})
	}
}

// copyCheck is the issue 25.1.4.7 and 26.2.1.14 give a rebuild's copy whose
// Unwrap meets a value that does not convert: PRECONDITION_FAILED with the
// check's message after the server's own words, measured.
func copyCheck(message string) *Ydb_Issue.IssueMessage {
	return issue(0, 0, 0, "Terminate was called, reason(141): <main>:1:202: Failed to unwrap empty optional:\n\n"+message+"\n\n\n")
}

// The check a rebuild's copy failed with is the provider's own sentence, and
// Read takes it from YDB's issue, nested or not, for each check the copy
// writes.
func TestReadTakesTheCheckARebuildCopyFailed_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, message string }{
		{"a value that does not convert", "rebuilding table app.items: column n holds a value that does not convert to its new type"},
		{"NULL or a value that does not convert", "rebuilding table app.items: column n holds NULL or a value that does not convert to its new type"},
		{"NULL in a column made NOT NULL", "rebuilding table items: column id holds NULL, and the new table declares it NOT NULL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			flat := ydbfailure.Read(&answered{status: Ydb.StatusIds_PRECONDITION_FAILED, issues: []*Ydb_Issue.IssueMessage{copyCheck(test.message)}})
			nested := ydbfailure.Read(&answered{status: Ydb.StatusIds_PRECONDITION_FAILED,
				issues: []*Ydb_Issue.IssueMessage{issue(1060, 0, 0, "Execution", copyCheck(test.message))}})
			c.Assert(flat.Check, qt.Equals, test.message)
			c.Assert(nested.Check, qt.Equals, test.message)
		})
	}
}

// Only a check the provider writes is taken: a sentence of the copy's grammar
// in any other issue, a failed Unwrap of another message, and an Unwrap of a
// sentence that is almost the copy's give no check.
func TestReadTakesTheCheckARebuildCopyFailed_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		issue *Ydb_Issue.IssueMessage
	}{
		{"outside a failed Unwrap", issue(0, 0, 0, "rebuilding table app.items: column n holds a value that does not convert to its new type")},
		{"another message", copyCheck("hunter2-secret")},
		{"another ending", copyCheck("rebuilding table app.items: column n holds hunter2-secret")},
		{"text after the check", copyCheck("rebuilding table app.items: column n holds a value that does not convert to its new type, hunter2-secret")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := ydbfailure.Read(&answered{status: Ydb.StatusIds_PRECONDITION_FAILED, issues: []*Ydb_Issue.IssueMessage{test.issue}})
			c.Assert(read.Check, qt.Equals, "")
		})
	}
}

// upsertOn262 is 26.2.1.14's answer to an UPSERT that names no column a
// synchronous index is keyed on, on a table with a unique index, as Read reads
// it: INTERNAL_ERROR from the planner, measured.
var upsertOn262 = located(answer(Ydb.StatusIds_INTERNAL_ERROR, "Execution", "Default error",
	"ydb/library/actors/core/log.cpp:881: verification=!hasUniqIndex || !usedIndexes.empty();fline=kqp_opt_phy_upsert_index.cpp:359;"), 1, 1, 165)

// uniqueIndexOn251 is 25.1.4.7's answer to a unique secondary index, as
// Read reads it: YDB refuses the index type inside the execution stage,
// before it proposes a schema operation.
var uniqueIndexOn251 = located(answer(Ydb.StatusIds_GENERIC_ERROR, "Execution", "Unknown index type: syncGlobalUnique"), 1060, 1, 56)

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
		{"generic scheme refused before its operation", uniqueIndexOn251, "ALTER TABLE t ADD INDEX u GLOBAL UNIQUE SYNC ON (v)", true},
		{"generic block refused while compiling", compiled(Ydb.StatusIds_GENERIC_ERROR), "SELECT 1; DO BEGIN SELECT 1; END DO", true},
		{"generic batch", answer(Ydb.StatusIds_GENERIC_ERROR), "BATCH UPDATE t SET v = 1", false},
		{"generic statistics", answer(Ydb.StatusIds_GENERIC_ERROR), "ANALYZE `t` (v)", true},
		{"internal while planning a data statement", upsertOn262, "UPSERT INTO t (id, plan) VALUES (2, 'trial'u)", true},
		{"internal while planning a block", upsertOn262, "DO BEGIN UPSERT INTO t (id, plan) VALUES (2, 'trial'u); END DO", false},
		{"internal while planning a batch", upsertOn262, "BATCH UPDATE t SET v = 1", false},
		{"internal of another kind", answer(Ydb.StatusIds_INTERNAL_ERROR, "Execution", "Default error"), "UPSERT INTO t (id) VALUES (1)", false},
		{"generic statistics after a definition", answer(Ydb.StatusIds_GENERIC_ERROR), "$t = 1; ANALYZE `t`", true},
		{"generic statistics beside a scheme statement", answer(Ydb.StatusIds_GENERIC_ERROR), "ANALYZE `t`; DROP TABLE `t`", false},
		{"generic scheme statement before statistics", answer(Ydb.StatusIds_GENERIC_ERROR), "CREATE TABLE t (id Int64, PRIMARY KEY (id)); ANALYZE `t`", false},
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
		{"scheme statement refused before its operation", uniqueIndexOn251, "ALTER TABLE t ADD INDEX u GLOBAL UNIQUE SYNC ON (v)", false, true},
		{"scheme block refused before its operation", uniqueIndexOn251, "DO BEGIN ALTER TABLE t ADD INDEX u GLOBAL UNIQUE SYNC ON (v); END DO", false, false},
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
	const (
		refusedDuplicate = "YDB refused the statement (PRECONDITION_FAILED)"
		aborted          = "YDB aborted the transaction for a conflict (ABORTED)"
		unknownDuplicate = "the unit's outcome is unknown: YDB answered PRECONDITION_FAILED, which does not show whether the unit applied"
	)
	for _, test := range []struct {
		name    string
		answer  ydbfailure.Answer
		unit    ydbfailure.Unit
		applied uint32
		want    *protocol.Execution
		reason  string
	}{
		{"rejected", duplicate, ydbfailure.Unit{Text: insert}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "Conflict with existing key."}, refusedDuplicate},
		{"rejected after a prefix", duplicate, ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 2,
			&protocol.Execution{Outcome: protocol.Execution_PARTIAL, AppliedCount: 2, DiagnosticCode: "ydb.statement-rejected", Message: "Conflict with existing key."}, refusedDuplicate},
		{"rejected while compiling", located(compiled(Ydb.StatusIds_GENERIC_ERROR, "Unknown simple type 'NoSuchType'"), 0, 1, 27),
			ydbfailure.Unit{Text: "CREATE TABLE t (id NoSuchType NOT NULL, PRIMARY KEY (id))"}, 2,
			&protocol.Execution{Outcome: protocol.Execution_PARTIAL, AppliedCount: 2, DiagnosticCode: "ydb.statement-rejected", Message: "Unknown simple type 'NoSuchType'"},
			"YDB refused the statement while compiling it (GENERIC_ERROR, line 1, column 27)"},
		{"refused before its operation", uniqueIndexOn251, ydbfailure.Unit{Text: "ALTER TABLE t ADD INDEX u GLOBAL UNIQUE SYNC ON (v)"}, 1,
			&protocol.Execution{Outcome: protocol.Execution_PARTIAL, AppliedCount: 1, DiagnosticCode: "ydb.statement-rejected", Message: "Execution; Unknown index type: syncGlobalUnique"},
			`YDB refused the statement, saying "Unknown index type: syncGlobalUnique": the YDB release the cluster runs has no unique secondary index ` +
				`(GENERIC_ERROR, issue 1060, line 1, column 56)`},
		{"refused while running", answer(Ydb.StatusIds_GENERIC_ERROR, "Scheme operation failed"), ydbfailure.Unit{Text: "CREATE TOPIC t"}, 0, unknown,
			"the unit's outcome is unknown: YDB answered GENERIC_ERROR, which does not show whether the unit applied"},
		// 26.2.1.14 refuses ANALYZE in its execution stage with default flags.
		{"statistics refused while running", answer(Ydb.StatusIds_GENERIC_ERROR, "Execution",
			"ANALYZE command is not supported because `EnableColumnStatistics` feature flag is off"), ydbfailure.Unit{Text: "ANALYZE `t`"}, 1,
			&protocol.Execution{Outcome: protocol.Execution_PARTIAL, AppliedCount: 1, DiagnosticCode: "ydb.statement-rejected",
				Message: "Execution; ANALYZE command is not supported because `EnableColumnStatistics` feature flag is off"},
			"YDB refused the statement (GENERIC_ERROR)"},
		{"statistics unconfirmed", answer(Ydb.StatusIds_UNDETERMINED), ydbfailure.Unit{Text: "ANALYZE `t`"}, 0, unknown,
			"the unit's outcome is unknown: YDB answered UNDETERMINED, which does not show whether the unit applied"},
		{"a rebuild check whose reason would not fit", ydbfailure.Answer{Status: Ydb.StatusIds_PRECONDITION_FAILED, Issues: []string{"Terminate was called"},
			Check: "rebuilding table " + strings.Repeat("t", 500) + ": column n holds a value that does not convert to its new type"},
			ydbfailure.Unit{Text: insert}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "Terminate was called"}, refusedDuplicate},
		{"internal while planning", upsertOn262, ydbfailure.Unit{Text: "UPSERT INTO t (id, plan) VALUES (2, 'trial'u)"}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "Execution; Default error; " +
				"ydb/library/actors/core/log.cpp:881: verification=!hasUniqIndex || !usedIndexes.empty();fline=kqp_opt_phy_upsert_index.cpp:359;"},
			"YDB refused the statement: YDB cannot plan an UPSERT on a table with a unique index when the statement names no column a synchronous index is keyed on; " +
				"name every column of the table, or use UPDATE or INSERT (INTERNAL_ERROR, issue 1, line 1, column 165)"},
		{"internal of another kind", answer(Ydb.StatusIds_INTERNAL_ERROR, "Execution"), ydbfailure.Unit{Text: insert}, 0, unknown,
			"the unit's outcome is unknown: YDB answered INTERNAL_ERROR, which does not show whether the unit applied"},
		{"rejected without issues", answer(Ydb.StatusIds_SCHEME_ERROR), ydbfailure.Unit{Text: "DROP TABLE t"}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "YDB rejected the unit"},
			"YDB refused the statement (SCHEME_ERROR)"},
		{"aborted", conflict, ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.transaction-aborted", Message: "Transaction locks invalidated. Table: `/local/t`.", TransactionAborted: true},
			aborted},
		{"aborted, not negotiated", conflict, ydbfailure.Unit{Text: insert, Transaction: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.transaction-aborted", Message: "Transaction locks invalidated. Table: `/local/t`."}, aborted},
		{"aborted after a prefix", conflict, ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 1,
			&protocol.Execution{Outcome: protocol.Execution_PARTIAL, AppliedCount: 1, DiagnosticCode: "ydb.transaction-aborted", Message: "Transaction locks invalidated. Table: `/local/t`."}, aborted},
		{"aborted without issues", answer(Ydb.StatusIds_ABORTED), ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.transaction-aborted", Message: "YDB aborted the transaction for a conflict", TransactionAborted: true},
			aborted},
		{"aborted for its size", sizeAbort, ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "Datashard program size limit exceeded (56281509 > 50331648)"},
			"YDB refused the statement: its writes do not fit one datashard program, which holds about 48 MiB; write the rows in smaller transactions (ABORTED, issue 200509)"},
		{"aborted outside a transaction", conflict, ydbfailure.Unit{Text: insert, Aborts: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "Transaction locks invalidated. Table: `/local/t`."},
			"YDB refused the statement (ABORTED)"},
		{"native", duplicate, ydbfailure.Unit{}, 0, unknown, unknownDuplicate},
		// A comment statement is one AlterTable, which YDB applies whole or
		// not at all.
		{"a call refused", answer(Ydb.StatusIds_BAD_REQUEST, "Error: user attributes too big: 10318"), ydbfailure.Unit{Call: true}, 0,
			&protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, DiagnosticCode: "ydb.statement-rejected", Message: "Error: user attributes too big: 10318"},
			"YDB refused the statement (BAD_REQUEST)"},
		{"a call refused after a prefix", answer(Ydb.StatusIds_SCHEME_ERROR), ydbfailure.Unit{Call: true}, 1,
			&protocol.Execution{Outcome: protocol.Execution_PARTIAL, AppliedCount: 1, DiagnosticCode: "ydb.statement-rejected", Message: "YDB rejected the unit"},
			"YDB refused the statement (SCHEME_ERROR)"},
		{"a call that failed for a reason that proves nothing", answer(Ydb.StatusIds_GENERIC_ERROR, "Scheme operation failed"), ydbfailure.Unit{Call: true}, 0, unknown,
			"the unit's outcome is unknown: YDB answered GENERIC_ERROR, which does not show whether the unit applied"},
		{"an unconfirmed call", answer(Ydb.StatusIds_UNAVAILABLE), ydbfailure.Unit{Call: true}, 0, unknown,
			"the unit's outcome is unknown: YDB answered UNAVAILABLE, which does not show whether the unit applied"},
		{"service statement in a transaction", duplicate, ydbfailure.Unit{Transaction: true, Aborts: true}, 0, unknown, unknownDuplicate},
		{"not applied whole", duplicate, ydbfailure.Unit{Text: "BATCH DELETE FROM t WHERE id > 0"}, 0, unknown, unknownDuplicate},
		{"unconfirmed", answer(Ydb.StatusIds_UNDETERMINED), ydbfailure.Unit{Text: insert, Transaction: true, Aborts: true}, 0, unknown,
			"the unit's outcome is unknown: YDB answered UNDETERMINED, which does not show whether the unit applied"},
		{"unconfirmed after a prefix", answer(Ydb.StatusIds_TIMEOUT), ydbfailure.Unit{Text: insert}, 1, unknownAfterOne,
			"the unit's outcome is unknown: YDB answered TIMEOUT, which does not show whether the unit applied"},
		{"no answer", ydbfailure.Answer{}, ydbfailure.Unit{Text: insert}, 0, unknown, "the unit's outcome is unknown: no answer of YDB's shows whether it applied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			want := proto.CloneOf(test.want)
			want.Diagnostic = &protocol.ProviderDiagnostic{Reason: test.reason}
			c.Assert(ydbfailure.Failed(test.answer, test.unit, test.applied), qt.CmpEquals(protocmp.Transform()), want)
		})
	}
}

func TestCommitOfAnAbortedTransactionIsRolledBack(t *testing.T) {
	for _, test := range []struct {
		name   string
		aborts bool
		want   *protocol.TransactionResult
	}{
		{"negotiated", true, &protocol.TransactionResult{Outcome: protocol.TransactionResult_ROLLED_BACK, Aborted: true, DiagnosticCode: "ydb.transaction-aborted",
			Message: "Transaction locks invalidated.", Diagnostic: &protocol.ProviderDiagnostic{Reason: "YDB aborted the transaction for a conflict (ABORTED)"}}},
		{"not negotiated", false, &protocol.TransactionResult{Outcome: protocol.TransactionResult_ROLLED_BACK, DiagnosticCode: "ydb.transaction-aborted",
			Message: "Transaction locks invalidated.", Diagnostic: &protocol.ProviderDiagnostic{Reason: "YDB aborted the transaction for a conflict (ABORTED)"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbfailure.Commit(answer(Ydb.StatusIds_ABORTED, "Transaction locks invalidated."), test.aborts), qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

// A commit YDB aborted for the transaction's size applied nothing either, so
// it is rolled back, but it is not aborted for a conflict: a host that runs
// aborted transactions again would get the same answer each time.
func TestCommitOfATransactionTooLargeIsRefused(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(ydbfailure.Commit(sizeAbort, true), qt.CmpEquals(protocmp.Transform()), &protocol.TransactionResult{Outcome: protocol.TransactionResult_ROLLED_BACK,
		DiagnosticCode: "ydb.transaction-rejected", Message: "Datashard program size limit exceeded (56281509 > 50331648)",
		Diagnostic: &protocol.ProviderDiagnostic{Reason: "YDB refused the transaction at its commit: its writes do not fit one datashard program, which holds about 48 MiB; " +
			"write the rows in smaller transactions (ABORTED, issue 200509)"}})
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

// The answers below are the ones 25.1.4.7 and 26.2.1.14 gave, measured, for
// statements YDB refused. Each issue text quotes a token of the statement, a
// literal in it or the table's path; the reason says what YDB did with the
// status, the issue code and the position, and of YDB's words it carries only
// a fixed sentence that quotes none of them.
func TestRefusedReasonQuotesNothingOfTheAnswer(t *testing.T) {
	for _, test := range []struct {
		name   string
		status Ydb.StatusIds_StatusCode
		issues []*Ydb_Issue.IssueMessage
		text   string
		want   string
		quoted string
	}{
		{"an unknown type", Ydb.StatusIds_GENERIC_ERROR, []*Ydb_Issue.IssueMessage{issue(0, 1, 27, "Unknown simple type 'NoSuchType'")},
			"CREATE TABLE `g75/t1` (id NoSuchType NOT NULL, PRIMARY KEY (id))",
			"YDB refused the statement while compiling it (GENERIC_ERROR, line 1, column 27)", "NoSuchType"},
		{"a parse error at a literal", Ydb.StatusIds_GENERIC_ERROR,
			[]*Ydb_Issue.IssueMessage{issue(0, 1, 43, "extraneous input ''extra'' expecting {<EOF>, ';'}")},
			"CREATE USER g75u PASSWORD 'hunter2-secret' 'extra'",
			"YDB refused the statement while compiling it (GENERIC_ERROR, line 1, column 43)", "extra"},
		{"a key the table holds, 26.2", Ydb.StatusIds_PRECONDITION_FAILED, []*Ydb_Issue.IssueMessage{
			issue(2012, 0, 0, "Constraint violated. Table: `/local/g75/t2`.", issue(2012, 0, 0, "Conflict with existing key."))},
			"INSERT INTO `g75/t2` (id, v) VALUES (1, 'hunter2-secret'u)",
			"YDB refused the statement: it violates a constraint of the table, such as a key the table holds (PRECONDITION_FAILED, issue 2012)", "/local"},
		{"a key the table holds, 25.1", Ydb.StatusIds_PRECONDITION_FAILED, []*Ydb_Issue.IssueMessage{issue(2012, 0, 0, "Conflict with existing key.")},
			"INSERT INTO `g75/t2` (id, v) VALUES (1, 'hunter2-secret'u)",
			"YDB refused the statement: it violates a constraint of the table, such as a key the table holds (PRECONDITION_FAILED, issue 2012)", "Conflict"},
		{"a NOT NULL column without a value", Ydb.StatusIds_BAD_REQUEST, []*Ydb_Issue.IssueMessage{issue(1030, 0, 0, "Type annotation",
			issue(0, 1, 13, "At function: KiWriteTable!", issue(2032, 1, 13, "Missing not null column in input: v. All not null columns should be initialized")))},
			"INSERT INTO `g75/t2` (id) VALUES (2)",
			"YDB refused the statement while compiling it: it gives no value to a NOT NULL column (BAD_REQUEST, issue 2032, line 1, column 13)", "column in input: v"},
		{"a value of another type", Ydb.StatusIds_GENERIC_ERROR, []*Ydb_Issue.IssueMessage{issue(1030, 0, 0, "Type annotation",
			issue(0, 1, 13, "At function: KiWriteTable!",
				issue(0, 1, 27, "Failed to convert type: Struct<'id':String,'v':Utf8> to Struct<'id':Int64,'v':Utf8>", issue(0, 1, 27, "Failed to convert 'id': String to Int64")),
				issue(2031, 1, 27, "Failed to convert input columns types to scheme types")))},
			"UPSERT INTO `g75/t2` (id, v) VALUES ('hunter2-secret', 'x'u)",
			"YDB refused the statement while compiling it: a value it writes does not convert to its column's type (GENERIC_ERROR, issue 2031, line 1, column 27)", "Struct<"},
		{"a table that does not exist", Ydb.StatusIds_SCHEME_ERROR, []*Ydb_Issue.IssueMessage{issue(1030, 0, 0, "Type annotation",
			issue(0, 1, 12, "At function: KiDropTable!", issue(2003, 1, 12, "Cannot find table 'db.[/local/g75/missing]' because it does not exist or you do not have access permissions.")))},
			"DROP TABLE `g75/missing`",
			"YDB refused the statement while compiling it: it names a scheme object that does not exist or that the connection may not read (SCHEME_ERROR, issue 2003, line 1, column 12)",
			"/local"},
		// The copy's check is the provider's own sentence, which names the
		// table and the column, as the host's embedded engine printed it; the
		// server's words around it stay in the message.
		{"a rebuild copy's check", Ydb.StatusIds_PRECONDITION_FAILED,
			[]*Ydb_Issue.IssueMessage{copyCheck("rebuilding table g75.items: column n holds a value that does not convert to its new type")},
			"INSERT INTO `g75/__ptah_rebuild_items` (`id`, `n`) SELECT `id` AS `id`, IF(`n` IS NULL, NULL, Unwrap(CAST(`n` AS Int64), " +
				"'rebuilding table g75.items: column n holds a value that does not convert to its new type'u)) AS `n` FROM `g75/items`;",
			"YDB refused the statement: rebuilding table g75.items: column n holds a value that does not convert to its new type (PRECONDITION_FAILED)",
			"Terminate was called"},
		{"an UPSERT the planner fails, 26.2", Ydb.StatusIds_INTERNAL_ERROR, []*Ydb_Issue.IssueMessage{issue(1060, 0, 0, "Execution", issue(0, 0, 0, "Default error",
			issue(1, 1, 165, "ydb/library/actors/core/log.cpp:881: verification=!hasUniqIndex || !usedIndexes.empty();fline=kqp_opt_phy_upsert_index.cpp:359;")))},
			"UPSERT INTO `g75/accounts` (`id`, `plan`) VALUES (2l, 'hunter2-secret'u)",
			"YDB refused the statement: YDB cannot plan an UPSERT on a table with a unique index when the statement names no column a synchronous index is keyed on; " +
				"name every column of the table, or use UPDATE or INSERT (INTERNAL_ERROR, issue 1, line 1, column 165)",
			"kqp_opt_phy_upsert_index"},
		// 26.2 refuses a secret whose name holds a space; the issue quotes the
		// path and the name, and the reason carries YDB's sentence without
		// them, as the host's embedded engine printed it.
		{"a name YDB does not take in a path", Ydb.StatusIds_SCHEME_ERROR, []*Ydb_Issue.IssueMessage{
			issue(2003, 0, 0, "Executing ESchemeOpCreateSecret", issue(2003, 0, 0,
				"Check failed: path: '/local/g75/bad name', error: symbol ' ' is not allowed in the path part 'bad name'")),
			issue(2019, 0, 0, "Query invalidated on scheme/internal error during Scheme execution")},
			"CREATE SECRET `g75/bad name` WITH (value = $PTAH_SECRET_HUNTER2)",
			`YDB refused the statement, saying "symbol ' ' is not allowed in the path part" (SCHEME_ERROR, issue 2019)`, "bad name"},
		// 25.1 has no unique secondary index: YDB's sentence names the index
		// type, not the statement's index.
		{"a unique index 25.1 does not know", Ydb.StatusIds_GENERIC_ERROR, []*Ydb_Issue.IssueMessage{
			issue(1060, 0, 0, "Execution", issue(0, 1, 56, "Unknown index type: syncGlobalUnique"))},
			"ALTER TABLE `g75/hunter2` ADD INDEX hunter2_u GLOBAL UNIQUE SYNC ON (v)",
			`YDB refused the statement, saying "Unknown index type: syncGlobalUnique": the YDB release the cluster runs has no unique secondary index ` +
				`(GENERIC_ERROR, issue 1060, line 1, column 56)`, "Execution"},
		{"a password the server refuses", Ydb.StatusIds_PRECONDITION_FAILED, []*Ydb_Issue.IssueMessage{issue(2029, 0, 0, "Executing ESchemeOpAlterLogin",
			issue(2029, 0, 0, "Kikimr precondition failed", issue(0, 0, 0, "Password contains unacceptable characters")))},
			"CREATE USER g75u PASSWORD 'hunter2-secret'",
			"YDB refused the statement (PRECONDITION_FAILED, issue 2029)", "Password"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := ydbfailure.Read(fmt.Errorf("execute: %w", &answered{status: test.status, issues: test.issues}))

			result := ydbfailure.Failed(read, ydbfailure.Unit{Text: test.text}, 0)

			c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
			c.Assert(result.GetDiagnostic().GetReason(), qt.Equals, test.want)
			c.Assert(result.GetMessage(), qt.Contains, test.quoted)
			c.Assert(result.GetDiagnostic().GetReason(), qt.Not(qt.Contains), test.quoted)
			c.Assert(result.GetDiagnostic().GetReason(), qt.Not(qt.Contains), "hunter2")
			c.Assert(provider.ValidateDiagnostic(result.GetDiagnostic()), qt.IsNil)
		})
	}
}

// A unit sent with lines in front of it, as a session on a dev realm sends
// one, has its position counted from its own text, and a position inside the
// lines in front is no position of the unit's.
func TestWithoutLinesCountsFromTheUnitsText(t *testing.T) {
	for _, test := range []struct {
		name                string
		row, column         uint32
		wantRow, wantColumn uint32
	}{
		{"a position in the unit", 2, 26, 1, 26},
		{"a position in the lines in front", 1, 9, 0, 0},
		{"no position", 0, 0, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			moved := located(compiled(Ydb.StatusIds_GENERIC_ERROR), 0, test.row, test.column).WithoutLines(1)
			c.Assert([]uint32{moved.Row, moved.Column}, qt.DeepEquals, []uint32{test.wantRow, test.wantColumn})
		})
	}
}

// flagSentence is the reason of an answer whose refusal says flag is off,
// around the provider's own reason.
func flagSentence(key, flag, reason string) string {
	return "capability " + key + " is off on this YDB cluster, which runs with feature flag " + flag + " off: " + reason +
		". Turn the flag on, or name the cluster's monitoring endpoint in the URL (monitoring=http://host:8765) so Ptah reads the flags before it plans"
}

// A refusal YDB answers because a feature flag is off names the capability and
// the flag, in the host's embedded engine's words, around the provider's own
// reason where the host's engine quotes YDB's text. The reason carries YDB's
// sentence for the flag, without what YDB wrote around it. The texts are
// measured ones: an index rename on a cluster with EnableMoveIndex off, and a
// resource pool on one with EnableResourcePools off, which every line has by
// default.
func TestRefusedReasonNamesTheFlag_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer ydbfailure.Answer
		want   string
	}{
		{name: "a refused statement", answer: ydbfailure.Answer{Status: Ydb.StatusIds_PRECONDITION_FAILED, Issues: []string{"Execution failed", "Move index is not supported yet"}},
			want: flagSentence("index_rename", "EnableMoveIndex", `YDB refused the statement, saying "Move index is not supported yet" (PRECONDITION_FAILED)`)},
		{name: "a statement refused while YDB compiled it", answer: ydbfailure.Answer{Status: Ydb.StatusIds_GENERIC_ERROR, Compiled: true, Code: 1030, Row: 1, Column: 1,
			Issues: []string{"Resource pools are disabled. Please contact your system administrator"}},
			want: flagSentence("resource_pools", "EnableResourcePools",
				`YDB refused the statement while compiling it, saying "Resource pools are disabled" (GENERIC_ERROR, issue 1030, line 1, column 1)`)},
		// A session that read the cluster's flags names the monitoring
		// endpoint already, so the sentence does not ask for it.
		{name: "a session that read the flags", answer: ydbfailure.Answer{Status: Ydb.StatusIds_PRECONDITION_FAILED, Issues: []string{"Execution failed", "Move index is not supported yet"}, FlagsRead: true},
			want: "capability index_rename is off on this YDB cluster, which runs with feature flag EnableMoveIndex off: " +
				`YDB refused the statement, saying "Move index is not supported yet" (PRECONDITION_FAILED). Turn the flag on`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got := test.answer.RefusedReason()
			c.Assert(got, qt.Equals, test.want)
			c.Assert(provider.ValidateDiagnostic(&protocol.ProviderDiagnostic{Reason: got}), qt.IsNil)
		})
	}
}

// A unit whose outcome is unknown says which flag YDB refused it for too.
func TestUnknownReasonNamesTheFlag_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	unknown := ydbfailure.Answer{Status: Ydb.StatusIds_GENERIC_ERROR, Issues: []string{"Streaming queries are disabled"}}
	c.Assert(unknown.UnknownReason(), qt.Equals, flagSentence("streaming_queries", "EnableStreamingQueries",
		"the unit's outcome is unknown: YDB answered GENERIC_ERROR, which does not show whether the unit applied"))
}

// An answer whose issues say nothing of a flag keeps the provider's reason as
// it is.
func TestRefusedReasonNamesTheFlag_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	refused := ydbfailure.Answer{Status: Ydb.StatusIds_PRECONDITION_FAILED, Code: 2012, Issues: []string{"Constraint violated", "Conflict with existing key."}}
	c.Assert(refused.RefusedReason(), qt.Equals, "YDB refused the statement: it violates a constraint of the table, such as a key the table holds (PRECONDITION_FAILED, issue 2012)")
}

// A reason carries only YDB's sentence as it was measured: a symbol YDB wrote
// as a control character, and the index type's sentence with more text after
// it, are not carried, and the reason is the provider's alone.
func TestRefusedReasonCarriesOnlyYDBsFixedTexts_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		answer ydbfailure.Answer
		want   string
	}{
		{"a symbol outside printable ASCII", located(answer(Ydb.StatusIds_SCHEME_ERROR, "Executing ESchemeOpCreateSecret",
			"Check failed: path: '/local/g75/a\x01b', error: symbol '\x01' is not allowed in the path part 'a\x01b'"), 2019, 0, 0),
			"YDB refused the statement (SCHEME_ERROR, issue 2019)"},
		{"the index type's sentence with more after it", located(answer(Ydb.StatusIds_GENERIC_ERROR, "Execution",
			"Unknown index type: syncGlobalUnique in hunter2"), 1060, 1, 56),
			"YDB refused the statement (GENERIC_ERROR, issue 1060, line 1, column 56)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(test.answer.RefusedReason(), qt.Equals, test.want)
		})
	}
}

// YDB refuses a DROP of an object other objects depend on before it changes
// anything, as 25.1.4.7 and 26.2.1.14 answered a DROP EXTERNAL DATA SOURCE an
// external table reads: SCHEME_ERROR, with the dependents named inside issue
// 2019. The unit is NOT_APPLIED, and the reason names the dependents, as the
// built-in engine at host 74de0b65b printed them.
func TestFailedRefusesADropOthersDependOn_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name   string
		issues []*Ydb_Issue.IssueMessage
	}{
		{"26.2", []*Ydb_Issue.IssueMessage{
			issue(2003, 0, 0, `Executing operation with object "EXTERNAL_DATA_SOURCE"`,
				issue(2003, 0, 0, "Failed to execute ESchemeOpDropExternalDataSource: <main>: Error: Other entities depend on this data source, "+
					"please remove them at the beginning: /local/g134x/tbl, code: 2003\n")),
			issue(2019, 0, 0, "Query invalidated on scheme/internal error during Scheme execution"),
		}},
		{"25.1", []*Ydb_Issue.IssueMessage{
			issue(2003, 0, 0, "<main>: Error: Other entities depend on this data source, please remove them at the beginning: /local/g134x/tbl, code: 2003\n"),
			issue(2019, 0, 0, "Query invalidated on scheme/internal error during Scheme execution"),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			read := ydbfailure.Read(&answered{status: Ydb.StatusIds_SCHEME_ERROR, issues: test.issues})

			result := ydbfailure.Failed(read, ydbfailure.Unit{Text: "DROP EXTERNAL DATA SOURCE `g134/src`"}, 0)

			c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
			c.Assert(result.GetDiagnostic().GetReason(), qt.Equals,
				"YDB refused the statement: other scheme objects depend on what it drops, /local/g134x/tbl; drop them first (SCHEME_ERROR, issue 2019)")
		})
	}
}

// Without the statement the answer says nothing of what the unit applied: a
// native unit that ran other statements first stays UNKNOWN, and its reason
// gives the answer's status alone.
func TestFailedRefusesADropOthersDependOn_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	read := ydbfailure.Read(&answered{status: Ydb.StatusIds_SCHEME_ERROR, issues: []*Ydb_Issue.IssueMessage{
		issue(2003, 0, 0, "<main>: Error: Other entities depend on this data source, please remove them at the beginning: /local/g134x/tbl, code: 2003\n"),
		issue(2019, 0, 0, "Query invalidated on scheme/internal error during Scheme execution"),
	}})

	result := ydbfailure.Failed(read, ydbfailure.Unit{}, 0)

	c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_UNKNOWN)
	c.Assert(result.GetDiagnostic().GetReason(), qt.Equals,
		"the unit's outcome is unknown: YDB answered SCHEME_ERROR, issue 2019, which does not show whether the unit applied")
}

// A call the gRPC layer refused before YDB took it ran nothing, so its unit
// is NOT_APPLIED, or PARTIAL after an applied prefix, with a reason that names
// which refusal it was: a request larger than the driver sends, which the
// driver refuses before it leaves (measured on 25.1.4.7 and 26.2.1.14 with a
// statement of 70 MiB), and a call the server's gRPC layer denied. The
// host's embedded engine read both as an outcome it knew. Ported from the
// YDB rows of the host's TestUnknown_OutcomeKnown.
func TestFailedReadsACallTheGRPCLayerRefused_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		err          error
		applied      uint32
		want         protocol.Execution_Outcome
	}{
		{name: "a request too large to send", err: fmt.Errorf("execute: %w", status.Error(codes.ResourceExhausted, "trying to send message larger than max (73400430 vs. 64000000)")),
			reason: "the unit never left: it is larger than the YDB driver sends in one request (gRPC ResourceExhausted)", want: protocol.Execution_NOT_APPLIED},
		{name: "a call the gRPC layer denied", err: status.Error(codes.PermissionDenied, "access denied"),
			reason: "YDB's gRPC layer denied the call before YDB ran it (gRPC PermissionDenied)", want: protocol.Execution_NOT_APPLIED},
		{name: "after an applied prefix", err: status.Error(codes.PermissionDenied, "access denied"), applied: 2,
			reason: "YDB's gRPC layer denied the call before YDB ran it (gRPC PermissionDenied)", want: protocol.Execution_PARTIAL},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result := ydbfailure.Failed(ydbfailure.Read(test.err), ydbfailure.Unit{Text: "CREATE TABLE t (id Uint64, PRIMARY KEY (id))"}, test.applied)

			c.Assert(result.GetOutcome(), qt.Equals, test.want)
			c.Assert(result.GetAppliedCount(), qt.Equals, test.applied)
			c.Assert(result.GetDiagnosticCode(), qt.Equals, "ydb.call-refused")
			c.Assert(result.GetDiagnostic().GetReason(), qt.Equals, test.reason)
			c.Assert(provider.ValidateDiagnostic(result.GetDiagnostic()), qt.IsNil)
		})
	}
}

// A transport failure that may come after YDB ran the unit leaves its outcome
// unknown: a response larger than the driver takes, which YDB sends after it
// ran the unit (the host's embedded engine read it as an outcome it knew,
// which it was not), a server out of resources, and a connection lost.
func TestFailedReadsACallTheGRPCLayerRefused_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "a response too large to receive", err: status.Error(codes.ResourceExhausted, "grpc: received message larger than max (73400430 vs. 64000000)")},
		{name: "a server out of resources", err: status.Error(codes.ResourceExhausted, "resource exhausted")},
		{name: "a connection lost", err: status.Error(codes.Unavailable, "connection reset by peer")},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result := ydbfailure.Failed(ydbfailure.Read(test.err), ydbfailure.Unit{Text: "CREATE TABLE t (id Uint64, PRIMARY KEY (id))"}, 0)

			c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_UNKNOWN)
			c.Assert(result.GetDiagnosticCode(), qt.Equals, "ydb.execution-unconfirmed")
		})
	}
}
