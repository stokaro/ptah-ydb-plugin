package backend

import (
	"context"

	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// AnalyzeSQL provides offline YQL framing and native safety judgments.
func (*Provider) AnalyzeSQL(ctx context.Context, request *protocol.AnalyzeSQLRequest) (*protocol.SQLAnalysis, error) {
	return yql.Analyze(ctx, request)
}
