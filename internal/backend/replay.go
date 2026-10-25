package backend

import (
	"context"
	"errors"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/status"
)

// GuardReplay judges each replay statement offline, as the host's YDB replay
// guard does; see yql.GuardReplay. A run that owns the whole server, by the
// operator's declaration or because it provisioned the server, may change
// anything on it but what reaches past it. Every other scope, the user
// databases of a server named as a whole and a pinned replay session
// included, confines a statement to the dev database the session selected.
func (p *Provider) GuardReplay(ctx context.Context, request *protocol.GuardReplayRequest) (*protocol.GuardedReplay, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	result := &protocol.GuardedReplay{Verdicts: make([]*protocol.ReplayVerdict, len(request.GetStatements()))}
	for i, statement := range request.GetStatements() {
		verdict := &protocol.ReplayVerdict{}
		var refusal *yql.RealmRefusal
		if errors.As(yql.GuardReplay(statement, request.GetScope().GetServer()), &refusal) {
			verdict.Refused, verdict.Operation = true, refusal.Operation
		}
		result.Verdicts[i] = verdict
	}
	return result, nil
}
