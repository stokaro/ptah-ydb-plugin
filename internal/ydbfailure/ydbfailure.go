// Package ydbfailure reads what YDB answered a failed query with, and says what
// the answer proves about the query: that YDB refused it and applied nothing of
// it, that YDB aborted the transaction that held it for a conflict, or neither.
//
// The provider's Execute, FinishTransaction and RunStatement replies are built
// from it, so a unit the server rejected is NOT_APPLIED rather than UNKNOWN,
// and a transaction aborted for a conflict is one the host may run again. The
// message of each such reply is the server's own issues, never the SDK's
// error text, which names connection addresses.
package ydbfailure

import (
	"errors"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"
)

// Answer is what YDB answered a failed call with.
type Answer struct {
	// Status is the operation status YDB answered with. It is
	// STATUS_CODE_UNSPECIFIED for an error that carries none: a transport
	// failure, a canceled call, or an error the provider raised itself.
	Status Ydb.StatusIds_StatusCode
	// Issues are the messages of the issues YDB attached, in order, each
	// issue before the issues nested in it.
	Issues []string
	// Compiled reports an answer in which YDB refused the text while
	// compiling it, before any of it ran: an issue names the position in the
	// text of what YDB refused, and no issue comes from the stage that
	// executes a query.
	Compiled bool
}

// operation is the SDK's error for an operation status YDB returned: the
// status and the issues YDB attached to it. The SDK's transport error has a
// status code too, but no issues, so it is not one.
type operation interface {
	error
	Code() int32
	Issues() []*Ydb_Issue.IssueMessage
}

// Read returns what YDB answered with err, or the zero Answer for an error
// that is not an operation status YDB returned.
func Read(err error) Answer {
	answered, ok := errors.AsType[operation](err)
	if !ok {
		return Answer{}
	}
	answer := Answer{Status: Ydb.StatusIds_StatusCode(answered.Code())}
	var positioned, executing bool
	var walk func([]*Ydb_Issue.IssueMessage)
	walk = func(issues []*Ydb_Issue.IssueMessage) {
		for _, issue := range issues {
			if issue.GetMessage() != "" {
				answer.Issues = append(answer.Issues, issue.GetMessage())
			}
			positioned = positioned || issue.GetPosition().GetRow() > 0
			executing = executing || executionIssue(issue)
			walk(issue.GetIssues())
		}
	}
	walk(answered.Issues())
	answer.Compiled = positioned && !executing
	return answer
}

// executionIssueCode is the code YQL gives the issue of its execution stage
// (TIssuesIds::CORE_EXEC), which holds what a query reported while it ran.
const executionIssueCode = 1060

// executionIssue reports an issue of the stage that executes a query: the
// execution stage's own, or a schema operation's, which YDB words as
// `Executing ESchemeOp...`. Measured on 25.1.4.7 and 26.2.1.14, such an issue
// may hold another that names a position in the text, as 25.1 answers
// `Unknown index type` for a unique index, so a position alone does not place
// the refusal before execution.
func executionIssue(issue *Ydb_Issue.IssueMessage) bool {
	return issue.GetIssueCode() == executionIssueCode || strings.HasPrefix(issue.GetMessage(), "Executing ")
}

// refusals are the statuses that establish a failure of the statement itself:
// YDB refused it, or aborted the transaction that held it. A status that says
// the call or the server failed, such as UNAVAILABLE, TIMEOUT or UNDETERMINED,
// establishes nothing about the statement.
var refusals = []Ydb.StatusIds_StatusCode{
	Ydb.StatusIds_BAD_REQUEST, Ydb.StatusIds_SCHEME_ERROR, Ydb.StatusIds_PRECONDITION_FAILED, Ydb.StatusIds_ALREADY_EXISTS,
	Ydb.StatusIds_NOT_FOUND, Ydb.StatusIds_UNSUPPORTED, Ydb.StatusIds_ABORTED,
}

// Confirmed reports an answer that establishes that the statement text failed:
// a status in which YDB refused it or aborted its transaction, or
// GENERIC_ERROR for a data statement the query service runs in one implicit
// transaction, or for any text YDB refused while compiling it. GENERIC_ERROR
// is also what YDB answers a compound statement, a batch write or transaction
// control that may have applied a prefix, so for those it establishes nothing
// unless YDB refused the text before any of it ran.
func (a Answer) Confirmed(text string) bool {
	return slices.Contains(refusals, a.Status) || a.Status == Ydb.StatusIds_GENERIC_ERROR && (atomicDataStatement(text) || a.Compiled)
}

// Rejected reports an answer that proves nothing of a unit of text was
// applied: it is [Answer.Confirmed], and the unit ran in a caller transaction,
// which YDB applies only by its commit, or runs as one query YDB applies
// whole. That is a data query, which is one transaction, or one scheme
// statement. A block or an action call that runs scheme statements runs them
// one by one, and a BATCH statement applies its rows in batches, so a failure
// of either may follow a part it applied.
func (a Answer) Rejected(text string, inTransaction bool) bool {
	return a.Confirmed(text) && (inTransaction || appliedWhole(text))
}

// Aborted reports a transaction YDB aborted without applying it, which YDB
// answers with ABORTED. Measured on 26.2.1.14, a commit and a write that follow
// a conflicting commit are both aborted with `Transaction locks invalidated`.
// A new attempt may succeed.
func (a Answer) Aborted() bool {
	return a.Status == Ydb.StatusIds_ABORTED
}

// PathBusy reports an answer that says another schema operation holds the
// path a schema statement acts on, such as the initial scan of a changefeed
// added to the table a moment before: OVERLOADED, naming the path as under
// operation or as locked by the operation's transaction (`path ... has been
// locked by tx`). YDB refuses the statement before it starts, so it changed
// nothing and may run again once that operation is done.
func (a Answer) PathBusy() bool {
	return a.Status == Ydb.StatusIds_OVERLOADED && slices.ContainsFunc(a.Issues, func(issue string) bool {
		return strings.Contains(issue, "path is under operation") || strings.Contains(issue, "has been locked by tx")
	})
}

// Message is the answer's issues on one line, or fallback when YDB attached
// none.
func (a Answer) Message(fallback string) string {
	issues := make([]string, 0, len(a.Issues))
	for _, issue := range a.Issues {
		if issue = strings.TrimSpace(issue); issue != "" {
			issues = append(issues, issue)
		}
	}
	if message := rowdata.OneLine(strings.Join(issues, "; ")); message != "" {
		return message
	}
	return fallback
}

// Unit is a unit of an Execute request that failed after it was dispatched.
type Unit struct {
	// Text is the unit's SQL, or empty for a unit that runs as more than a
	// query YDB's query service answers for: a native unit, whose operations
	// the provider runs one by one, and a unit that holds a comment or
	// coordination node statement.
	Text string
	// Call reports a unit whose failure is the answer to one call of a YDB
	// service other than the query service, which YDB applies whole or not at
	// all, with nothing of the unit applied before it: a comment statement,
	// which sets one user attribute with one AlterTable.
	Call bool
	// Transaction reports a unit that ran in a caller transaction.
	Transaction bool
	// Aborts reports a host that listed units.migration-split, which carries
	// Execution.transaction_aborted.
	Aborts bool
}

// Fixed messages of the replies that carry no answer of the server's.
const (
	unknownMessage  = "The dispatched unit's outcome is unknown; no automatic retry was attempted"
	rejectedMessage = "YDB rejected the unit"
	abortedMessage  = "YDB aborted the transaction for a conflict"
)

// Failed is the reply to an Execute whose first applied units were applied and
// whose next unit failed after dispatch with answer. A unit YDB rejected
// without applying any of it is NOT_APPLIED, or PARTIAL after an applied
// prefix, with the server's issues as its message: a unit of text whose
// answer is [Answer.Rejected], and a call whose answer is a status in which
// YDB refuses it. One that YDB aborted for a conflict in a caller
// transaction, as the request's first unit, also sets transaction_aborted
// when the host negotiated it. Anything else is UNKNOWN.
func Failed(answer Answer, unit Unit, applied uint32) *protocol.Execution {
	rejected := unit.Text != "" && answer.Rejected(unit.Text, unit.Transaction) || unit.Call && slices.Contains(refusals, answer.Status)
	if !rejected {
		return &protocol.Execution{Outcome: protocol.Execution_UNKNOWN, AppliedCount: applied, DiagnosticCode: "ydb.execution-unconfirmed", Message: unknownMessage}
	}
	result := &protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, AppliedCount: applied, DiagnosticCode: "ydb.statement-rejected", Message: answer.Message(rejectedMessage)}
	if applied > 0 {
		result.Outcome = protocol.Execution_PARTIAL
	}
	if answer.Aborted() && unit.Transaction {
		result.DiagnosticCode, result.Message = "ydb.transaction-aborted", answer.Message(abortedMessage)
		result.TransactionAborted = unit.Aborts && applied == 0
	}
	return result
}

// Commit is the outcome of a COMMIT that failed with answer, or nil when the
// failure leaves the outcome unknown. A transaction YDB aborted for a conflict
// applied nothing: it is ROLLED_BACK, with aborted set when the host
// negotiated it.
func Commit(answer Answer, aborts bool) *protocol.TransactionResult {
	if !answer.Aborted() {
		return nil
	}
	return &protocol.TransactionResult{Outcome: protocol.TransactionResult_ROLLED_BACK, Aborted: aborts,
		DiagnosticCode: "ydb.transaction-aborted", Message: answer.Message(abortedMessage)}
}

// appliedWhole reports a unit of text that YDB applies whole or not at all:
// one data query, or one scheme statement that is neither a block nor an
// action call.
func appliedWhole(text string) bool {
	queries, err := yqlquery.Split(text)
	if err != nil || len(queries) != 1 {
		return false
	}
	switch queries[0].Kind {
	case yqlquery.Data:
		return true
	case yqlquery.Scheme:
		return !opensBlock(queries[0].Statements[0])
	default:
		return false
	}
}

// opensBlock reports a statement that is a block or an action call: DO or
// EVALUATE.
func opensBlock(statement string) bool {
	for _, token := range yql.Tokens(statement) {
		switch token.Type {
		case yql.TokenWhitespace, yql.TokenComment, yql.TokenUnknown:
			continue
		}
		return token.IsWord("DO") || token.IsWord("EVALUATE")
	}
	return false
}

// atomicDataStatement recognizes data statements the query service executes in
// one implicit transaction. Compound actions, batch writes and transaction
// control may expose a successful prefix and cannot establish failed-without-
// changes from a generic server status.
func atomicDataStatement(text string) bool {
	first := ""
	for _, token := range yql.Tokens(text) {
		if token.Type != yql.TokenIdentifier {
			continue
		}
		word := strings.ToUpper(token.Value)
		if first == "" {
			first = word
		}
		if slices.Contains([]string{"COMMIT", "ROLLBACK", "BATCH", "DEFINE", "DO", "EVALUATE", "IMPORT", "PROCESS", "REDUCE", "COMBINE"}, word) {
			return false
		}
	}
	return slices.Contains([]string{"SELECT", "INSERT", "UPSERT", "REPLACE", "UPDATE", "DELETE"}, first)
}
