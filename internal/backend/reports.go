package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ReportValues counts captured values offline for `ptah schema stats`: each
// value, under its ID and in order, with one count for each metric of its
// model's report (schema.Reports). It reads no database. A value that does
// not validate fails the whole call.
func (*Provider) ReportValues(ctx context.Context, request *protocol.ReportValuesRequest) (*protocol.ValueReports, error) {
	result := &protocol.ValueReports{Complete: true}
	for _, input := range request.GetValues() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		counts, err := schema.CountValue(input.GetValue(), input.GetRepresentation())
		if err != nil {
			return nil, authored.Status(codes.InvalidArgument, err)
		}
		result.Values = append(result.Values, &protocol.ValueReport{Id: input.GetId(), Counts: counts})
	}
	return result, nil
}
