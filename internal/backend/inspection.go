package backend

import (
	"bytes"
	"context"

	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Inspect serializes inspection with close on this session only. A closed
// handle cannot reopen a connection or fall back to an embedded engine.
func (p *Provider) Inspect(ctx context.Context, request *protocol.InspectRequest) (*protocol.Schema, error) {
	p.mu.Lock()
	session := p.sessions[request.GetSessionId()]
	p.mu.Unlock()
	if session == nil {
		return nil, status.Error(codes.NotFound, "unknown YDB session")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return nil, status.Error(codes.FailedPrecondition, "YDB session is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	result, err := inspect.Read(ctx, session.connection.Driver, session.database, request.GetSchemas())
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if _, err := provider.InspectionValues(Description(), request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}

// NormalizeValues validates captured YDB observation facets offline. Results
// preserve the complete input provenance and expose no successful prefix.
func (*Provider) NormalizeValues(ctx context.Context, request *protocol.NormalizeValuesRequest) (*protocol.NormalizedValues, error) {
	if err := provider.ValidateValueInputs(Description(), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result := &protocol.NormalizedValues{Complete: true}
	for _, input := range request.GetValues() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		if input.GetRepresentation() != protocol.ValueInput_OBSERVED {
			return nil, status.Error(codes.InvalidArgument, "only observed YDB facets are implemented")
		}
		value, err := schema.Normalize(input.GetValue())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		result.Values = append(result.Values, &protocol.NormalizedValue{Id: input.GetId(), Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
			CanonicalFormat: value.Kind + "/observed-json-v1", CanonicalProjection: bytes.Clone(value.Payload)})
	}
	return result, nil
}
