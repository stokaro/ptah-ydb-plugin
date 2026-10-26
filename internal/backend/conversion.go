package backend

import (
	"context"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ConvertValues converts a complete batch offline. Source normalization is
// recomputed from payloads; a caller-supplied projection is never authoritative.
func (p *Provider) ConvertValues(ctx context.Context, request *protocol.ConvertValuesRequest) (*protocol.NormalizedValues, error) {
	if err := provider.ValidateConversionRequest(Description(), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	source := &protocol.NormalizeValuesRequest{Engine: request.Engine, TargetFeatures: request.TargetFeatures}
	for _, value := range request.Values {
		source.Values = append(source.Values, &protocol.ValueInput{Id: value.Id, Representation: request.From, Value: proto.CloneOf(value.Value)})
	}
	normalized, err := p.NormalizeValues(ctx, source)
	if err != nil {
		return nil, err
	}
	for i, value := range normalized.Values {
		if !proto.Equal(value, request.Values[i]) {
			return nil, status.Error(codes.InvalidArgument, "YDB conversion source normalization changed")
		}
	}
	destination := &protocol.NormalizeValuesRequest{Engine: request.Engine, TargetFeatures: request.TargetFeatures}
	for _, value := range normalized.Values {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		var converted *protocol.Envelope
		var err error
		switch value.Value.Kind {
		case schema.ColumnProperties:
			converted, err = schema.ConvertColumn(value.Value, request.From, request.To)
		case schema.TableProperties:
			converted, err = schema.ConvertTable(value.Value, request.From, request.To)
		case schema.SecretProperties, schema.IndexProperties, schema.ChangefeedProperties, schema.TopicProperties, schema.ViewProperties, schema.ExternalSourceProperties, schema.ExternalTableProperties, schema.ColumnStorageProperties,
			schema.ResourcePoolProperties, schema.ResourcePoolClassifierProperties:
			converted, err = schema.ConvertNative(value.Value, request.From, request.To)
		case schema.CoordinationProperties:
			converted, err = schema.ConvertCoordination(value.Value, request.From, request.To)
		default:
			return nil, status.Error(codes.Unimplemented, "YDB model has no representation conversion")
		}
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		destination.Values = append(destination.Values, &protocol.ValueInput{Id: value.Id, Representation: request.To, Value: converted})
	}
	result, err := p.NormalizeValues(ctx, destination)
	if err != nil {
		return nil, err
	}
	if err := provider.ValidateConvertedValues(Description(), request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}
