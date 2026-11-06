package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/migrationunits"
	"github.com/stokaro/ptah-ydb-plugin/internal/rowdata"
)

var _ provider.MigrationSplitter = (*Provider)(nil)

// SplitMigration splits a YDB migration body offline into the queries YDB
// runs it as, each placed where the host runs it, or refuses the body whole
// (see internal/migrationunits). Each check assertion gets the verdict
// RunStatement gives its text under READ_ONLY before it opens anything, so a
// dry run refuses what the run would; the run still proves the reads local
// against the catalog. An unusable request, cancellation or a reply the SDK
// would not accept returns an error and no unit.
func (*Provider) SplitMigration(ctx context.Context, request *protocol.SplitMigrationRequest) (*protocol.MigrationSplit, error) {
	description := provider.NegotiatedDescription(described(), provider.HostFeatures(ctx))
	if err := provider.ValidateSplitMigrationRequest(description, request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result := &protocol.MigrationSplit{}
	result.Units, result.Refusal = migrationunits.Split(request.GetBody())
	for _, assertion := range request.GetAssertions() {
		verdict := &protocol.AssertionVerdict{ReadOnly: true}
		if _, reason := statementText(ctx, assertion, protocol.RunStatementRequest_READ_ONLY); reason != "" {
			verdict = &protocol.AssertionVerdict{Reason: rowdata.OneLine(reason)}
		}
		result.Assertions = append(result.Assertions, verdict)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidateMigrationSplit(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	return result, nil
}
