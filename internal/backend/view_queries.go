package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbview"
)

// CompareViewQueries answers, for each declared view paired with the view the
// database holds under its name, whether the two are one query on YDB
// (schema.view-query-equivalence, contract 25.33). Both queries are read into
// the form the server stores a view's query in (see ydbview.QueryText) and
// compared as they stand, as the host's embedded engine compared them. YDB
// names keep case, so a query that changes only the case of a name is
// another query; the host's shared reading folds case and reports no change
// for it. A comment, a line break or a terminator the server drops changes
// nothing. The call reads the request alone.
func (*Provider) CompareViewQueries(ctx context.Context, request *protocol.CompareViewQueriesRequest) (*protocol.ComparedViewQueries, error) {
	description := described()
	if err := provider.ValidateCompareViewQueriesRequest(description, request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result := &protocol.ComparedViewQueries{Complete: true}
	for _, pair := range request.GetPairs() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		held := pair.GetCurrent()
		result.Queries = append(result.Queries, &protocol.ComparedViewQuery{Identity: proto.CloneOf(held.GetIdentity()),
			Equivalent: ydbview.QueryText(pair.GetDeclared()) == ydbview.QueryText(held.GetBody())})
	}
	if err := provider.ValidateComparedViewQueriesReply(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	return result, nil
}
