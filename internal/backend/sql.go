package backend

import (
	"context"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// AnalyzeSQL provides offline YQL framing and native safety judgments.
func (*Provider) AnalyzeSQL(ctx context.Context, request *protocol.AnalyzeSQLRequest) (*protocol.SQLAnalysis, error) {
	return yql.Analyze(ctx, request)
}
