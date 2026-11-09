package backend

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/reversal"
)

// ReverseChanges undoes the standalone object changes the comparisons
// returned, offline. Each reverse asks for the declaration of what the
// forward change found and predicts what the forward change leaves; the
// prediction is normalized as an observed value is.
func (p *Provider) ReverseChanges(ctx context.Context, request *protocol.ReverseChangesRequest) (*protocol.ReversedChanges, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := described()
	if err := provider.ValidateReverseChangesRequest(description, request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if !proto.Equal(request.GetIdentifiers().GetRules(), identifierRules()) {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "YDB change reversal requires its negotiated identifier rules")
	}
	observe := func(id string, value *protocol.Envelope) (*protocol.NormalizedValue, error) {
		normalized, err := p.NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: request.GetEngine(), TargetFeatures: request.GetTargetFeatures(),
			Values: []*protocol.ValueInput{{Id: id, Representation: protocol.ValueInput_OBSERVED, Value: value}}})
		if err != nil {
			return nil, err
		}
		return normalized.GetValues()[0], nil
	}
	result, err := reversal.Reverse(ctx, request, observe)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		if _, isStatus := status.FromError(err); isStatus {
			return nil, err
		}
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if err := provider.ValidateReversedChangesReply(description, request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}
