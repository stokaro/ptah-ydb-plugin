package backend

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// statementFailure carries only server issues, never the SDK error wrapper
// that includes connection addresses and authentication details. Only statuses
// establishing rejection or transaction abort count as a confirmed failure.
func statementFailure(id string, err error, text string) *protocol.StatementResult {
	result := &protocol.StatementResult{RequestId: id, Outcome: protocol.StatementResult_UNKNOWN}
	if !ydb.IsOperationError(err, Ydb.StatusIds_BAD_REQUEST, Ydb.StatusIds_SCHEME_ERROR,
		Ydb.StatusIds_PRECONDITION_FAILED, Ydb.StatusIds_ALREADY_EXISTS, Ydb.StatusIds_NOT_FOUND,
		Ydb.StatusIds_UNSUPPORTED, Ydb.StatusIds_ABORTED) &&
		!(ydb.IsOperationError(err, Ydb.StatusIds_GENERIC_ERROR) && atomicDataStatement(text)) {
		return result
	}
	var issues []string
	ydb.IterateByIssues(err, func(message string, _ Ydb.StatusIds_StatusCode, _ uint32) {
		if message != "" {
			issues = append(issues, message)
		}
	})
	message := strings.ToValidUTF8(strings.ReplaceAll(strings.Join(issues, "\n"), "\x00", ""), "")
	if message == "" {
		message = "YDB rejected the statement"
	}
	if len(message) > provider.MaxStatementErrorBytes {
		message = message[:provider.MaxStatementErrorBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	result.Outcome = protocol.StatementResult_FAILED
	result.Error = message
	result.ErrorCode = Ydb.StatusIds_StatusCode(ydb.OperationError(err).Code()).String()
	return result
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
