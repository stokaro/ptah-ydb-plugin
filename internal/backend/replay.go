package backend

import (
	"context"
	"errors"

	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// GuardReplay judges each replay statement offline, as the host's YDB replay
// guard does; see yql.GuardReplay. A run that owns the whole server, by the
// operator's declaration or because it provisioned the server, may change
// anything on it but what reaches past it. Every other scope, the user
// databases of a server named as a whole and a pinned replay session
// included, confines a statement to the dev database the session selected.
//
// A trusted scope lifts the refusals whose only reason is the scope, and
// keeps the ones that protect the session and its cleanup; see
// yql.GuardTrustedReplay. Trust never stands for a server the run owns: the
// scope's server field alone selects the owned server's guard.
func (p *Provider) GuardReplay(ctx context.Context, request *protocol.GuardReplayRequest) (*protocol.GuardedReplay, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	scope := request.GetScope()
	guard := yql.GuardReplay
	switch {
	case scope.GetServer() && scope.GetTrustedSql():
		guard = yql.GuardTrustedServerReplay
	case scope.GetServer():
		guard = yql.GuardServerReplay
	case scope.GetTrustedSql():
		guard = yql.GuardTrustedReplay
	}
	result := &protocol.GuardedReplay{Verdicts: make([]*protocol.ReplayVerdict, len(request.GetStatements()))}
	for i, statement := range request.GetStatements() {
		verdict := &protocol.ReplayVerdict{}
		if refusal, ok := errors.AsType[*yql.RealmRefusal](guard(statement)); ok {
			verdict.Refused, verdict.Operation = true, refusal.Operation
		}
		result.Verdicts[i] = verdict
	}
	return result, nil
}
