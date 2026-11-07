// Package ydbfailure reads what YDB answered a failed query with, and says what
// the answer proves about the query: that YDB refused it and applied nothing of
// it, that YDB aborted the transaction that held it for a conflict, or neither.
//
// The provider's Execute, FinishTransaction and RunStatement replies are built
// from it, so a unit the server rejected is NOT_APPLIED rather than UNKNOWN,
// and a transaction aborted for a conflict is one the host may run again. The
// message of each such reply is the server's own issues, never the SDK's
// error text, which names connection addresses. No host prints the message.
// The reason of an Execute or FinishTransaction reply, which a host that
// lists errors.reasons prints, is a sentence of the provider's own: what YDB
// did, its status, and the issue code and the position YDB gave, with the
// capability and the feature flag where YDB refused for want of the flag. It
// never repeats YDB's issue text as a whole, which quotes the statement's
// text, a literal in it or the database's path. The parts of it a reason
// repeats are the fixed sentences YDB writes, which quote none of those (see
// [Answer.said]), as the host's embedded engine printed them, and the paths of
// the objects YDB says depend on one a statement drops, which the operator
// has to drop first.
package ydbfailure

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Issue"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/featureflags"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
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
	// Code is the most specific issue code YDB gave: that of the last issue,
	// in the order of Issues, whose code is not 0. It is 0 when no issue has
	// one.
	Code uint32
	// Row and Column are the most specific position YDB named in the text:
	// those of the last issue, in the order of Issues, that names one. They
	// are 0 when no issue does.
	Row, Column uint32
	// FlagsRead reports a session that read the cluster's feature flags from
	// the monitoring endpoint its address names, rather than taking its
	// release line's default. Read leaves it false; the caller that knows
	// the session sets it.
	FlagsRead bool
	// Repeats reports an answer one of whose issues has a code a new attempt
	// of the transaction answers again (see [RepeatedAbort]): YDB aborted it
	// for its size, not for a conflict.
	Repeats bool
	// Check is the message of a check the provider wrote into the statement
	// and the statement failed: a rebuild's copy that meets a value its
	// column's new type cannot hold (see [rebuildCheck]). It is the
	// provider's own sentence, naming a table and a column and no value, so
	// a reason may carry it. It is "" for any other answer.
	Check string
	// Unrun is the reason a call that carries no YDB status ran nothing,
	// or "": the gRPC layer refused it before YDB took it (see
	// [unrunCall]).
	Unrun string
}

// unrunCall is the reason the gRPC call that failed with code and message
// ran nothing, or "" where nothing shows that. A request larger than the
// driver sends is refused before it leaves (`trying to send message larger
// than max`, the SDK's limit of 64000000 bytes, measured on 25.1.4.7 and
// 26.2.1.14), and a call the server's gRPC layer denies (PermissionDenied)
// is refused before YDB runs it. A response larger than the driver takes
// (`received message larger than max`) comes after YDB ran the unit, and
// any other transport failure may come before or after, so each stays
// unknown.
func unrunCall(code codes.Code, message string) string {
	switch {
	case code == codes.ResourceExhausted && strings.Contains(message, "trying to send message larger than max"):
		return "the unit never left: it is larger than the YDB driver sends in one request (gRPC ResourceExhausted)"
	case code == codes.PermissionDenied:
		return "YDB's gRPC layer denied the call before YDB ran it (gRPC PermissionDenied)"
	default:
		return ""
	}
}

// repeatedAborts are the issue codes of an ABORTED answer that YDB gives for
// the transaction itself, not for a conflict with another one, so every new
// attempt answers the same. Measured on 25.1.4.7, a transaction whose writes
// do not fit one datashard program commits with ABORTED and issue 200509,
// `Datashard program size limit exceeded (56281509 > 50331648)`.
var repeatedAborts = map[uint32]bool{200509: true}

// RepeatedAbort reports an issue code of an ABORTED answer that a new attempt
// of the transaction answers again.
func RepeatedAbort(code uint32) bool {
	return repeatedAborts[code]
}

// operation is the SDK's error for an operation status YDB returned: the
// status and the issues YDB attached to it. The SDK's transport error has a
// status code too, but no issues, so it is not one.
type operation interface {
	error
	Code() int32
	Issues() []*Ydb_Issue.IssueMessage
}

// Read returns what YDB answered with err. An error that is not an
// operation status YDB returned reads as the zero Answer, with the reason a
// gRPC status it carries shows that the call ran nothing (see [unrunCall]).
func Read(err error) Answer {
	answered, ok := errors.AsType[operation](err)
	if !ok {
		if transport, isStatus := status.FromError(err); isStatus && err != nil {
			return Answer{Unrun: unrunCall(transport.Code(), transport.Message())}
		}
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
			if check := rebuildCheckOf(issue.GetMessage()); check != "" {
				answer.Check = check
			}
			if issue.GetIssueCode() != 0 {
				answer.Code = issue.GetIssueCode()
			}
			answer.Repeats = answer.Repeats || RepeatedAbort(issue.GetIssueCode())
			if issue.GetPosition().GetRow() > 0 {
				positioned = true
				answer.Row, answer.Column = issue.GetPosition().GetRow(), issue.GetPosition().GetColumn()
			}
			executing = executing || executionIssue(issue)
			walk(issue.GetIssues())
		}
	}
	walk(answered.Issues())
	answer.Compiled = positioned && !executing
	return answer
}

// failedUnwrap is how YDB introduces the message of an Unwrap that met an
// empty optional, in the issue of a query that stopped there: `Terminate was
// called, reason(141): <main>:1:202: Failed to unwrap empty optional:`, then
// a blank line and the message, on 25.1.4.7 and 26.2.1.14 alike.
const failedUnwrap = "Failed to unwrap empty optional:"

// rebuildCheck is the grammar of the messages a rebuild's copy gives its
// Unwrap checks (tablecreate.CopyStatement), which name the table, as its
// directory and name joined by a dot, and the column. The host's embedded
// engine wrote the same checks and printed YDB's answer, so its user read
// which column stopped the copy; the provider's reason says the same.
var rebuildCheck = regexp.MustCompile(`(?m)^rebuilding table [^\n]+: column [^\n]+ holds ` +
	`(?:NULL, and the new table declares it NOT NULL|NULL or a value that does not convert to its new type|a value that does not convert to its new type)$`)

// rebuildCheckOf is the rebuild check message an issue of a failed Unwrap
// carries, or "" for any other issue.
func rebuildCheckOf(message string) string {
	_, after, found := strings.Cut(message, failedUnwrap)
	if !found {
		return ""
	}
	return rebuildCheck.FindString(after)
}

// executionIssueCode is the code YQL gives the issue of its execution stage
// (TIssuesIds::CORE_EXEC), which holds what a query reported while it ran.
const executionIssueCode = 1060

// unproposed are the measured messages of an issue in which YDB refused a
// schema statement while it turned the statement into a schema operation,
// before it proposed one, with what each says the statement asked for. The
// issue stands inside the execution stage's, so the answer is not one YDB
// gave while compiling the text, but no schema operation started, so nothing
// of the statement applied. 25.1.4.7 answers a unique secondary index this
// way, GENERIC_ERROR with `Unknown index type: syncGlobalUnique`; 26.2 knows
// the index type.
var unproposed = map[string]string{
	"Unknown index type: syncGlobalUnique": "the YDB release the cluster runs has no unique secondary index",
}

// plannerFailures are the measured issues in which YDB failed a data query
// while it planned it, for a defect of the server's, before any of it ran,
// with what each says the statement asked for. 26.2.1.14 answers an UPSERT
// that names no column a synchronous index of the table is keyed on, on a
// table with a unique index, with INTERNAL_ERROR and
// `verification=!hasUniqIndex || !usedIndexes.empty();fline=kqp_opt_phy_upsert_index.cpp:359`,
// and writes nothing; 25.1.4.7 runs the same statement. The key leaves the
// source line out, which another build of the server may move.
var plannerFailures = map[string]string{
	"verification=!hasUniqIndex || !usedIndexes.empty();fline=kqp_opt_phy_upsert_index.cpp:": "YDB cannot plan an UPSERT on a table with a unique index " +
		"when the statement names no column a synchronous index is keyed on; name every column of the table, or use UPDATE or INSERT",
}

// plannerFailure is the meaning of the answer's issue in which YDB failed a
// data query while it planned it, or "" for an answer with no such issue.
func (a Answer) plannerFailure() string {
	if a.Status != Ydb.StatusIds_INTERNAL_ERROR {
		return ""
	}
	for _, issue := range a.Issues {
		for marker, meaning := range plannerFailures {
			if strings.Contains(issue, marker) {
				return meaning
			}
		}
	}
	return ""
}

// fixedTexts are the sentences YDB writes into an issue that quote nothing a
// statement or a database holds, measured on 25.1.4.7 and 26.2.1.14, each as
// a pattern whose first group is the part a reason carries. The host's
// embedded engine printed YDB's issues whole, so its user read these words;
// the reason carries them alone.
var fixedTexts = []*regexp.Regexp{
	// 25.1.4.7 has no unique secondary index (see unproposed).
	regexp.MustCompile(`^(Unknown index type: syncGlobalUnique)$`),
	// A name YDB does not take in a path: `Check failed: path:
	// '/local/<dir>/bad name', error: symbol ' ' is not allowed in the path
	// part 'bad name'`, for CREATE SECRET on 26.2.1.14. The path and the
	// part are the statement's and are left out. YDB writes a symbol outside
	// ASCII as '?', so the symbol is one printable ASCII character.
	regexp.MustCompile(`(symbol '[[:print:]]' is not allowed in the path part)`),
}

// said is what YDB said in the answer in a sentence of its own that a reason
// may carry: the first of [fixedTexts] an issue holds, or else the text of a
// feature flag's refusal (see featureflags.RefusalText). It is "" for an
// answer with neither.
func (a Answer) said() string {
	for _, issue := range a.Issues {
		for _, text := range fixedTexts {
			if found := text.FindStringSubmatch(strings.TrimSpace(issue)); found != nil {
				return found[1]
			}
		}
	}
	return featureflags.RefusalText(a.Issues)
}

// refusedUnproposed is the meaning of the answer's issue that refused a
// schema statement before YDB proposed its operation, or "" for an answer
// with no such issue.
func (a Answer) refusedUnproposed() string {
	for _, issue := range a.Issues {
		if meaning, known := unproposed[strings.TrimSpace(issue)]; known {
			return meaning
		}
	}
	return ""
}

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
// transaction, for one ANALYZE statement, or for any text YDB refused while
// compiling it. GENERIC_ERROR is also what YDB answers a compound statement, a
// batch write or transaction control that may have applied a prefix, so for
// those it establishes nothing unless YDB refused the text before any of it
// ran. INTERNAL_ERROR establishes the failure only of a data statement the
// query service runs in one implicit transaction, and only where the issue is
// one YDB gives while it plans the query (see [plannerFailures]).
func (a Answer) Confirmed(text string) bool {
	return slices.Contains(refusals, a.Status) ||
		a.Status == Ydb.StatusIds_GENERIC_ERROR && (atomicDataStatement(text) || statisticsStatement(text) || a.Compiled || a.refusedUnproposed() != "") ||
		a.plannerFailure() != "" && atomicDataStatement(text)
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

// Aborted reports a transaction YDB aborted for a conflict without applying
// it, which YDB answers with ABORTED. Measured on 26.2.1.14, a commit and a
// write that follow a conflicting commit are both aborted with `Transaction
// locks invalidated`. A new attempt may succeed. An ABORTED answer that
// [Answer.Repeats] is not one: nothing of the transaction was applied either,
// and a new attempt answers the same.
func (a Answer) Aborted() bool {
	return a.Status == Ydb.StatusIds_ABORTED && !a.Repeats
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

// issueMeanings say what the statement did that YDB refused, for the issue
// codes measured on 25.1.4.7 and 26.2.1.14 whose meaning the code alone
// establishes. YDB's text for each quotes the table's path or a type the
// statement wrote, so the provider says it in its own words.
var issueMeanings = map[uint32]string{
	// `Cannot find table '...' because it does not exist or you do not have
	// access permissions`.
	2003: "it names a scheme object that does not exist or that the connection may not read",
	// `Constraint violated. Table: ...`, then `Conflict with existing key.`
	2012: "it violates a constraint of the table, such as a key the table holds",
	// `Failed to convert input columns types to scheme types`.
	2031: "a value it writes does not convert to its column's type",
	// `Missing not null column in input: ...`.
	2032: "it gives no value to a NOT NULL column",
	// `Datashard program size limit exceeded (56281509 > 50331648)`, at the
	// commit on 25.1.4.7.
	200509: "its writes do not fit one datashard program, which holds about 48 MiB; write the rows in smaller transactions",
}

// WithoutLines is a with its position counted from the unit's own text, for
// a unit the provider sent with lines lines in front of it, as a session on a
// dev realm sends the realm's pragma. A position inside those lines names no
// text of the unit and is dropped.
func (a Answer) WithoutLines(lines uint32) Answer {
	if a.Row <= lines {
		a.Row, a.Column = 0, 0
		return a
	}
	a.Row -= lines
	return a
}

// detail is the answer's status, then the most specific issue code and the
// most specific position YDB gave, when it gave them:
// `GENERIC_ERROR, issue 1030, line 1, column 27`.
func (a Answer) detail() string {
	parts := []string{a.Status.String()}
	if a.Code != 0 {
		parts = append(parts, fmt.Sprintf("issue %d", a.Code))
	}
	if a.Row > 0 {
		parts = append(parts, fmt.Sprintf("line %d, column %d", a.Row, a.Column))
	}
	return strings.Join(parts, ", ")
}

// RefusedReason is the reason of a reply to a unit YDB refused. It says that
// YDB refused the statement, whether while compiling it, what YDB said where
// it said it in a sentence of its own (see [Answer.said]), what the issue
// code means when the provider knows, and the detail, and names the feature
// flag the refusal says is off (see [Answer.flagged]).
func (a Answer) RefusedReason() string {
	return a.refusedReason("YDB refused the statement")
}

// dependentsIssue opens the issue in which YDB refuses to drop an object other
// objects depend on, and dependentsNamed the paths of those objects after it.
// Measured on 25.1.4.7 and 26.2.1.14, a DROP EXTERNAL DATA SOURCE an external
// table reads answers SCHEME_ERROR with `Other entities depend on this data
// source, please remove them at the beginning: /local/x/tbl, code: 2003` inside
// issue 2019, `Query invalidated on scheme/internal error during Scheme
// execution`, whose code alone says nothing.
const (
	dependentsIssue = "Other entities depend on this"
	dependentsNamed = "please remove them at the beginning: "
)

// refusedDependents is the meaning of the answer's issue that refused to drop
// an object others depend on, naming them where YDB does, or "" for an answer
// with no such issue.
func (a Answer) refusedDependents() string {
	for _, issue := range a.Issues {
		at := strings.Index(issue, dependentsIssue)
		if at < 0 {
			continue
		}
		_, named, found := strings.Cut(issue[at:], dependentsNamed)
		named, _, _ = strings.Cut(named, ", code:")
		if named = strings.TrimSpace(named); !found || named == "" {
			return "other scheme objects depend on what it drops; drop them first"
		}
		return "other scheme objects depend on what it drops, " + named + "; drop them first"
	}
	return ""
}

// refusedReason is [Answer.RefusedReason] with the refusal said as reason.
func (a Answer) refusedReason(reason string) string {
	if a.Compiled {
		reason += " while compiling it"
	}
	if said := a.said(); said != "" {
		reason += `, saying "` + said + `"`
	}
	switch meaning, known := issueMeanings[a.Code]; {
	case a.Check != "" && fits(reason+": "+a.Check+" ("+a.detail()+")"):
		reason += ": " + a.Check
	case a.refusedDependents() != "":
		reason += ": " + a.refusedDependents()
	case known:
		reason += ": " + meaning
	case a.refusedUnproposed() != "":
		reason += ": " + a.refusedUnproposed()
	case a.plannerFailure() != "":
		reason += ": " + a.plannerFailure()
	}
	return a.flagged(reason + " (" + a.detail() + ")")
}

// fits reports a reason a host may show: one line of at most 512 bytes with
// no control character (provider.ValidateDiagnostic). A rebuild check names
// a table and a column, which may be long or hold any character YDB takes in
// a name, so a reason that would not fit leaves it out.
func fits(reason string) bool {
	return provider.ValidateDiagnostic(&protocol.ProviderDiagnostic{Reason: reason}) == nil
}

// flagged is reason for an answer whose issues say a feature flag YDB keeps a
// capability behind is off, in the words the host's embedded engine writes
// for such a refusal: the capability, the flag, then reason where the host's
// engine quotes YDB's text, and what to do. A session reads the flags only
// when its address names the monitoring endpoint. One that took its release
// line's default planned with the capability on, and is told to name the
// endpoint as well; one that read the flags names it already, so turning the
// flag on is what remains. reason is returned as it is for any other answer.
func (a Answer) flagged(reason string) string {
	gate, off := featureflags.Refused(a.Issues)
	if !off {
		return reason
	}
	refused := fmt.Sprintf("capability %s is off on this YDB cluster, which runs with feature flag %s off: %s. Turn the flag on", gate.Key, gate.Flag, reason)
	if a.FlagsRead {
		return refused
	}
	return refused + ", or name the cluster's monitoring endpoint in the URL (monitoring=http://host:8765) so Ptah reads the flags before it plans"
}

// AbortedReason is the reason of a reply to a unit or a commit whose
// transaction YDB aborted for a conflict.
func (a Answer) AbortedReason() string {
	return "YDB aborted the transaction for a conflict (" + a.detail() + ")"
}

// UnknownReason is the reason of a reply to a unit whose outcome the answer
// does not establish. An answer without a status is a transport failure, a
// canceled call or an answer the provider did not keep, such as one to a unit
// that expanded a secret input.
func (a Answer) UnknownReason() string {
	if a.Status == Ydb.StatusIds_STATUS_CODE_UNSPECIFIED {
		return "the unit's outcome is unknown: no answer of YDB's shows whether it applied"
	}
	return a.flagged("the unit's outcome is unknown: YDB answered " + a.detail() + ", which does not show whether the unit applied")
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
	unrunMessage    = "The unit was refused before YDB ran it"
)

// Failed is the reply to an Execute whose first applied units were applied and
// whose next unit failed after dispatch with answer. A unit YDB rejected
// without applying any of it is NOT_APPLIED, or PARTIAL after an applied
// prefix, with the server's issues as its message: a unit of text whose
// answer is [Answer.Rejected], and a call whose answer is a status in which
// YDB refuses it. One that YDB aborted for a conflict in a caller
// transaction, as the request's first unit, also sets transaction_aborted
// when the host negotiated it. A call the gRPC layer refused before YDB ran
// it is NOT_APPLIED, or PARTIAL, too, with [Answer.Unrun] as its reason.
// Anything else is UNKNOWN. Each other reply's diagnostic gives its reason:
// [Answer.RefusedReason], [Answer.AbortedReason] or [Answer.UnknownReason].
func Failed(answer Answer, unit Unit, applied uint32) *protocol.Execution {
	if answer.Unrun != "" {
		result := &protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, AppliedCount: applied, DiagnosticCode: "ydb.call-refused", Message: unrunMessage,
			Diagnostic: &protocol.ProviderDiagnostic{Reason: answer.Unrun}}
		if applied > 0 {
			result.Outcome = protocol.Execution_PARTIAL
		}
		return result
	}
	rejected := unit.Text != "" && answer.Rejected(unit.Text, unit.Transaction) || unit.Call && slices.Contains(refusals, answer.Status)
	if !rejected {
		return &protocol.Execution{Outcome: protocol.Execution_UNKNOWN, AppliedCount: applied, DiagnosticCode: "ydb.execution-unconfirmed", Message: unknownMessage,
			Diagnostic: &protocol.ProviderDiagnostic{Reason: answer.UnknownReason()}}
	}
	result := &protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, AppliedCount: applied, DiagnosticCode: "ydb.statement-rejected", Message: answer.Message(rejectedMessage),
		Diagnostic: &protocol.ProviderDiagnostic{Reason: answer.RefusedReason()}}
	if applied > 0 {
		result.Outcome = protocol.Execution_PARTIAL
	}
	if answer.Aborted() && unit.Transaction {
		result.DiagnosticCode, result.Message = "ydb.transaction-aborted", answer.Message(abortedMessage)
		result.Diagnostic = &protocol.ProviderDiagnostic{Reason: answer.AbortedReason()}
		result.TransactionAborted = unit.Aborts && applied == 0
	}
	return result
}

// Commit is the outcome of a COMMIT that failed with answer, or nil when the
// failure leaves the outcome unknown. A transaction YDB aborted applied
// nothing: it is ROLLED_BACK. One aborted for a conflict sets aborted when the
// host negotiated it, so the host may run it again; one whose answer
// [Answer.Repeats] is refused, since a new attempt answers the same.
func Commit(answer Answer, aborts bool) *protocol.TransactionResult {
	switch {
	case answer.Aborted():
		return &protocol.TransactionResult{Outcome: protocol.TransactionResult_ROLLED_BACK, Aborted: aborts,
			DiagnosticCode: "ydb.transaction-aborted", Message: answer.Message(abortedMessage),
			Diagnostic: &protocol.ProviderDiagnostic{Reason: answer.AbortedReason()}}
	case answer.Status == Ydb.StatusIds_ABORTED:
		return &protocol.TransactionResult{Outcome: protocol.TransactionResult_ROLLED_BACK,
			DiagnosticCode: "ydb.transaction-rejected", Message: answer.Message(rejectedMessage),
			Diagnostic: &protocol.ProviderDiagnostic{Reason: answer.refusedReason("YDB refused the transaction at its commit")}}
	default:
		return nil
	}
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

// statisticsStatement reports text that is one ANALYZE statement. ANALYZE
// collects column statistics: it creates, changes and drops no schema object
// and writes no row, so a failed one applied nothing a plan or a migration
// changes, whichever stage of YDB refused it. Measured on 26.2.1.14 with
// default flags, YDB refuses it in its execution stage, GENERIC_ERROR with
// `ANALYZE command is not supported because EnableColumnStatistics feature
// flag is off` and no position in the text, where 25.1.4.7 refuses it while
// compiling (`analyze is not supported for oltp tables`).
func statisticsStatement(text string) bool {
	queries, err := yqlquery.Split(text)
	if err != nil || len(queries) != 1 || queries[0].Kind != yqlquery.Scheme || len(queries[0].Statements) != 1 {
		return false
	}
	for _, token := range yql.Tokens(queries[0].Statements[0]) {
		switch token.Type {
		case yql.TokenWhitespace, yql.TokenComment, yql.TokenUnknown:
			continue
		}
		return token.IsWord("ANALYZE")
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
