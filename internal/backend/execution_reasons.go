package backend

import (
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbview"
)

// realmPrefixLines is how many lines a session on a dev realm sends in front
// of every query: the realm's pragma. A position YDB names in such a query
// counts them, and the reason counts from the unit's own text.
var realmPrefixLines = uint32(strings.Count(ydbview.RealmPrefix("/"), "\n"))

// fixedReasons are the reasons of the execution results the provider builds
// before YDB answered, by diagnostic code. Each is a sentence the provider
// wrote, so a host that lists errors.reasons prints it; the result's message
// can repeat a parser's or a server's text and is never printed. A result whose
// reason depends on its outcome is in outcomeReason.
//
// #nosec G101 -- the keys are diagnostic codes and the values are sentences; none is a credential
var fixedReasons = map[string]string{
	"ydb.invalid-coordination-statement": "the unit holds a coordination node statement the provider cannot read",
	"ydb.invalid-comment-statement":      "the unit holds a comment statement the provider cannot read",
	"ydb.native-transaction-forbidden":   "a native unit, or a comment or coordination node statement, runs outside a transaction, as YDB runs every scheme statement",
	"ydb.unsupported-unit":               "the provider does not run this native unit",
	"ydb.invalid-native-unit":            "the native unit is malformed",
	"ydb.invalid-execution-inputs":       "the unit's execution inputs do not match its statement",
	"ydb.secrets-unsupported":            "the YDB server keeps no scheme secrets",
	"ydb.column-addition-unsupported":    "the YDB server cannot add a column with a default",
	"ydb.foreign-database":               "the native unit names a database other than the session's",
	"ydb.realm-escape":                   "the unit names a path outside the dev realm",
	"ydb.canceled-before-execution":      "the run was canceled before the first unit started",
	"ydb.canceled-before-unit":           "the run was canceled before the next unit started",
	"ydb.transaction-inactive":           "the transaction the unit names is not active",
	"ydb.transaction-active":             "the session has an active transaction, and the unit names none",
	"ydb.connection-unavailable":         "no YDB connection was available for the unit",
	"ydb.precondition-refused":           "the provider did not send the unit: its precondition or lock guarantee was not established",
}

// outcomeReason is the reason of a result whose diagnostic code the provider
// gives for more than one outcome, or empty for any other result.
func outcomeReason(result *protocol.Execution) string {
	unknown := result.GetOutcome() == protocol.Execution_UNKNOWN
	switch result.GetDiagnosticCode() {
	case "ydb.lock-guarantee-lost":
		if unknown {
			return "the session lost its lock while the units ran, so their outcome is unknown"
		}
		return "the session does not hold its lock, so no unit ran"
	case "ydb.statement-timeout":
		if unknown {
			return "the statement timeout stopped waiting for the unit, which YDB may still apply"
		}
		return "the statement timeout stopped the unit, and nothing of it was applied"
	default:
		return fixedReasons[result.GetDiagnosticCode()]
	}
}

// withReason gives a result that did not apply every unit the reason its
// diagnostic code names, unless it carries one already.
func withReason(result *protocol.Execution) *protocol.Execution {
	if result == nil || result.GetOutcome() == protocol.Execution_APPLIED || result.GetDiagnostic() != nil {
		return result
	}
	if reason := outcomeReason(result); reason != "" {
		result.Diagnostic = &protocol.ProviderDiagnostic{Reason: reason}
	}
	return result
}
