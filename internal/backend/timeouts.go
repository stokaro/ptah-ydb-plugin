package backend

import (
	"context"
	"time"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Operation_V1"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
)

// How a statement timeout stops a unit, as the host's embedded YDB engine
// answers for it (migration/migrator, ydb_timeouts.go).
//
// YDB has no statement timeout a session sets, so the provider bounds each
// unit with a deadline of its own and answers for what YDB does when it fires,
// which depends on the unit:
//
//   - A unit in the caller's transaction is canceled, and the transaction
//     never commits: nothing of the unit is applied. The provider rolls the
//     transaction back and keeps the outcome for the caller's finish.
//   - A schema statement YDB runs as a build -- an index added to a table that
//     exists, or a column added with a default -- keeps running when the client
//     stops waiting. The provider cancels the build through the operation
//     service and waits for it to end; a build it stopped leaves nothing
//     applied, and one that completed first is applied.
//   - Any other unit, and a build that cannot be found or does not end, may
//     still be applied by the server: its outcome is unknown.

// buildWaitMargin is the time a build cancellation leaves before the deadline
// of the Execute call, so the reply arrives before the host stops waiting.
const buildWaitMargin = 2 * time.Second

// timedOut answers for a unit of request the statement timeout stopped.
func (s *session) timedOut(ctx context.Context, request *protocol.ExecuteRequest) stoppedUnit {
	return func(unit *protocol.Unit) stopOutcome {
		if request.GetTransactionId() != "" {
			// The transaction can only be rolled back now, so it is, and
			// its outcome is retained for the caller's finish.
			if record := s.transactions[request.GetTransactionId()]; record != nil && record.tx != nil {
				record.failed = true
				rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				s.closeMetadataReaders(request.GetTransactionId(), false)
				s.finishTransaction(rollback, record, protocol.FinishTransactionRequest_ROLLBACK)
				// The driver answers the rollback that the session is not
				// valid for reuse, because the canceled query ended it. The
				// transaction never sent a commit, and a YDB transaction is
				// applied only by its commit, so it is rolled back either way.
				record.outcome = protocol.TransactionResult_ROLLED_BACK
				s.active = ""
			}
			return stopNotApplied
		}
		table, altered := yql.AlteredTable(unit.GetSql())
		if !altered {
			return stopUnknown
		}
		wait := nativeapi.DefaultBuildWait
		if deadline, bounded := ctx.Deadline(); bounded {
			wait.Settle = min(wait.Settle, time.Until(deadline)-buildWaitMargin-wait.Appear)
		}
		if wait.Settle <= 0 {
			return stopUnknown
		}
		builds := nativeapi.NewBuilds(Ydb_Operation_V1.NewOperationServiceClient(ydb.GRPCConn(s.connection.Driver)), s.database)
		outcome, err := builds.CancelRunning(ctx, table, wait)
		switch {
		case err != nil:
			return stopUnknown
		case outcome == nativeapi.BuildStopped:
			return stopNotApplied
		case outcome == nativeapi.BuildCompleted:
			return stopApplied
		default:
			return stopUnknown
		}
	}
}
