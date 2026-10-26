package backend

import (
	"strings"
	"unicode/utf8"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// statementFailure carries only server issues, never the SDK error wrapper
// that includes connection addresses and authentication details. Only statuses
// establishing rejection or transaction abort count as a confirmed failure.
func statementFailure(id string, err error) *protocol.StatementResult {
	result := &protocol.StatementResult{RequestId: id, Outcome: protocol.StatementResult_UNKNOWN}
	if !ydb.IsOperationError(err, Ydb.StatusIds_BAD_REQUEST, Ydb.StatusIds_SCHEME_ERROR,
		Ydb.StatusIds_PRECONDITION_FAILED, Ydb.StatusIds_ALREADY_EXISTS, Ydb.StatusIds_NOT_FOUND,
		Ydb.StatusIds_UNSUPPORTED, Ydb.StatusIds_ABORTED) { return result }
	var issues []string
	ydb.IterateByIssues(err, func(message string, _ Ydb.StatusIds_StatusCode, _ uint32) {
		if message != "" { issues = append(issues, message) }
	})
	message := strings.ToValidUTF8(strings.ReplaceAll(strings.Join(issues, "\n"), "\x00", ""), "")
	if message == "" { message = "YDB rejected the statement" }
	if len(message) > provider.MaxStatementErrorBytes {
		message = message[:provider.MaxStatementErrorBytes]
		for !utf8.ValidString(message) { message = message[:len(message)-1] }
	}
	result.Outcome = protocol.StatementResult_FAILED
	result.Error = message
	result.ErrorCode = Ydb.StatusIds_StatusCode(ydb.OperationError(err).Code()).String()
	return result
}
