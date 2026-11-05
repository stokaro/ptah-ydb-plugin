package backend

// These tests inject driver failures at the execution boundary, as
// execution_test.go does: a live server cannot lose a reply on demand. They
// read the provider's own reason table, which no public API lists.

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// answeredError is an operation error as YDB's SDK returns one: a status and
// the issues YDB attached to it.
type answeredError struct {
	status Ydb.StatusIds_StatusCode
	issues []*Ydb_Issue.IssueMessage
}

func (a *answeredError) Error() string                     { return "operation/" + a.status.String() + ": hunter2" }
func (a *answeredError) Code() int32                       { return int32(a.status) }
func (a *answeredError) Issues() []*Ydb_Issue.IssueMessage { return a.issues }

// Every reason the provider gives a result it builds itself is one a host may
// show.
func TestFixedExecutionReasonsAreShowable(t *testing.T) {
	c := testcheck.New(t)
	for code, reason := range fixedReasons {
		c.Assert(provider.ValidateDiagnostic(&protocol.ProviderDiagnostic{Reason: reason}), qt.IsNil, qt.Commentf("%s", code))
	}
	for _, code := range []string{"ydb.lock-guarantee-lost", "ydb.statement-timeout"} {
		for _, outcome := range []protocol.Execution_Outcome{protocol.Execution_NOT_STARTED, protocol.Execution_NOT_APPLIED, protocol.Execution_UNKNOWN} {
			reason := outcomeReason(&protocol.Execution{DiagnosticCode: code, Outcome: outcome})
			c.Assert(provider.ValidateDiagnostic(&protocol.ProviderDiagnostic{Reason: reason}), qt.IsNil, qt.Commentf("%s %s", code, outcome))
		}
	}
}

// A result that did not apply every unit says why: in a sentence of its own
// when the provider refused the unit before YDB answered, and from YDB's
// status and issue code when YDB answered. A commit after the refusal says
// why it rolled back. Nothing of the driver's text reaches a reason.
func TestExecutionResultsGiveTheirReason(t *testing.T) {
	t.Run("a transaction that is not active", func(t *testing.T) {
		c := testcheck.New(t)
		p := probeProvider(c, &driverProbe{})

		result, err := p.Execute(c.Context(), sqlRequest("tx"))

		c.Assert(err, qt.IsNil)
		c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_NOT_STARTED)
		c.Assert(result.GetDiagnostic().GetReason(), qt.Equals, "the transaction the unit names is not active")
	})
	t.Run("a lost reply", func(t *testing.T) {
		c := testcheck.New(t)
		p := probeProvider(c, &driverProbe{execErr: driver.ErrBadConn})

		result, err := p.Execute(c.Context(), sqlRequest(""))

		c.Assert(err, qt.IsNil)
		c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_UNKNOWN)
		c.Assert(result.GetDiagnostic().GetReason(), qt.Equals, "the unit's outcome is unknown: no answer of YDB's shows whether it applied")
	})
	t.Run("a unit YDB refused on a dev realm", func(t *testing.T) {
		c := testcheck.New(t)
		// The realm's pragma is the first line YDB received, so YDB names
		// the type on the second.
		refusal := &answeredError{status: Ydb.StatusIds_GENERIC_ERROR,
			issues: []*Ydb_Issue.IssueMessage{{Message: "Unknown simple type 'NoSuchType'", Position: &Ydb_Issue.IssueMessage_Position{Row: 2, Column: 26}}}}
		p := probeProvider(c, &driverProbe{execErr: refusal})
		p.sessions["session"].realm = true
		request := sqlRequest("")
		request.Units[0].Body = &protocol.Unit_Sql{Sql: "CREATE TABLE `items` (id NoSuchType NOT NULL, PRIMARY KEY (id))"}

		result, err := p.Execute(c.Context(), request)

		c.Assert(err, qt.IsNil)
		c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
		c.Assert(result.GetDiagnostic().GetReason(), qt.Equals, "YDB refused the statement while compiling it (GENERIC_ERROR, line 1, column 26)")
	})
	t.Run("a lock lost while the unit ran", func(t *testing.T) {
		c := testcheck.New(t)
		probe := &driverProbe{}
		p := probeProvider(c, probe)
		probe.onExec = func(context.Context) error {
			p.sessions["session"].fenced = true
			return driver.ErrBadConn
		}

		result, err := p.Execute(c.Context(), sqlRequest(""))

		c.Assert(err, qt.IsNil)
		c.Assert(result.GetOutcome(), qt.Equals, protocol.Execution_UNKNOWN)
		c.Assert(result.GetDiagnosticCode(), qt.Equals, "ydb.lock-guarantee-lost")
		c.Assert(result.GetDiagnostic().GetReason(), qt.Equals, "the session lost its lock while the units ran, so their outcome is unknown")
	})
	t.Run("a unit YDB refused in a transaction, and its commit", func(t *testing.T) {
		c := testcheck.New(t)
		refusal := &answeredError{status: Ydb.StatusIds_PRECONDITION_FAILED,
			issues: []*Ydb_Issue.IssueMessage{{IssueCode: 2012, Message: "Conflict with existing key."}}}
		p := probeProvider(c, &driverProbe{execErr: refusal})
		_, err := p.BeginTransaction(c.Context(), beginRequest())
		c.Assert(err, qt.IsNil)

		executed, err := p.Execute(c.Context(), sqlRequest("tx"))
		c.Assert(err, qt.IsNil)
		committed, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_COMMIT))
		c.Assert(err, qt.IsNil)

		c.Assert(executed.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
		c.Assert(executed.GetDiagnostic().GetReason(), qt.Equals,
			"YDB refused the statement: it violates a constraint of the table, such as a key the table holds (PRECONDITION_FAILED, issue 2012)")
		c.Assert(committed.GetOutcome(), qt.Equals, protocol.TransactionResult_ROLLED_BACK)
		c.Assert(committed.GetDiagnostic().GetReason(), qt.Equals, "the transaction was rolled back because a statement in it failed")
	})
	t.Run("a commit YDB did not confirm", func(t *testing.T) {
		c := testcheck.New(t)
		p := probeProvider(c, &driverProbe{commitErr: errors.New("hunter2")})
		_, err := p.BeginTransaction(c.Context(), beginRequest())
		c.Assert(err, qt.IsNil)

		committed, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_COMMIT))

		c.Assert(err, qt.IsNil)
		c.Assert(committed.GetOutcome(), qt.Equals, protocol.TransactionResult_UNKNOWN)
		c.Assert(committed.GetDiagnostic().GetReason(), qt.Equals,
			"YDB did not confirm the transaction's outcome, and the commit or rollback was not retried")
	})
}

// A rollback the caller asked for succeeds, so it gives no reason, even after
// a unit YDB refused.
func TestRequestedRollbackGivesNoReason(t *testing.T) {
	c := testcheck.New(t)
	refusal := &answeredError{status: Ydb.StatusIds_PRECONDITION_FAILED}
	p := probeProvider(c, &driverProbe{execErr: refusal})
	_, err := p.BeginTransaction(c.Context(), beginRequest())
	c.Assert(err, qt.IsNil)
	_, err = p.Execute(c.Context(), sqlRequest("tx"))
	c.Assert(err, qt.IsNil)

	rolledBack, err := p.FinishTransaction(c.Context(), finishRequest(protocol.FinishTransactionRequest_ROLLBACK))

	c.Assert(err, qt.IsNil)
	c.Assert(rolledBack.GetOutcome(), qt.Equals, protocol.TransactionResult_ROLLED_BACK)
	c.Assert(rolledBack.GetDiagnostic(), qt.IsNil)
}
