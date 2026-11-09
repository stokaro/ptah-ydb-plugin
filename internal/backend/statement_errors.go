package backend

import (
	"strings"
	"unicode/utf8"

	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbfailure"
)

// statementFailure carries only server issues, never the SDK error wrapper
// that includes connection addresses and authentication details. Only statuses
// establishing rejection or transaction abort count as a confirmed failure
// (see ydbfailure.Answer.Confirmed).
func statementFailure(id string, err error, text string) *protocol.StatementResult {
	result := &protocol.StatementResult{RequestId: id, Outcome: protocol.StatementResult_UNKNOWN}
	answer := ydbfailure.Read(err)
	if !answer.Confirmed(text) {
		return result
	}
	message := strings.ToValidUTF8(strings.ReplaceAll(strings.Join(answer.Issues, "\n"), "\x00", ""), "")
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
	result.ErrorCode = answer.Status.String()
	return result
}
