package backend

import (
	"bytes"
	"context"
	"net/url"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnstore"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
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
	var common inspect.Common
	if carriesCommon(provider.HostFeatures(ctx)) {
		common = inspect.Common{Views: true, Comments: true}
		if session.realm {
			common.Realm = session.database
		}
	}
	result, err := inspect.ReadObjects(ctx, session.connection.Driver, session.columnTables(), session.database, request.GetSchemas(), common, session.streaming(), session.leftOut()...)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	read, unread := session.workload()
	if err := inspect.Workload(ctx, result, session.database, request.GetSchemas(), read, unread); err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if carriesCommon(provider.HostFeatures(ctx)) {
		if slices.Contains(provider.HostFeatures(ctx), provider.CommonAccessFeature) {
			principals := func(ctx context.Context) (nativeapi.Principals, error) {
				return nativeapi.ReadPrincipals(ctx, session.connection.DB, session.connection.Database())
			}
			if err := inspect.Access(ctx, result, session.database, session.realm, nativeapi.AccessReader(session.connection.Driver), principals); err != nil {
				if ctx.Err() != nil {
					return nil, status.FromContextError(ctx.Err()).Err()
				}
				return nil, status.Error(codes.FailedPrecondition, err.Error())
			}
		} else {
			for _, kind := range []protocol.CommonObjectSupport_Kind{protocol.CommonObjectSupport_ROLE, protocol.CommonObjectSupport_GRANT} {
				result.CommonObjects.Coverage = append(result.CommonObjects.Coverage, &protocol.CommonObjectCoverage{Kind: kind,
					Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Access inspection requires schema.common-access"}})
			}
		}
	}
	result.TargetFeatures = negotiatedFeatures(ctx)
	// The reply is checked against the description the host holds: one that
	// did not list the common wire declares no common object.
	if _, err := provider.InspectionValues(provider.NegotiatedDescription(Description(), provider.HostFeatures(ctx)), request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}

func (s *session) streaming() inspect.StreamingQuery {
	if !s.capabilities[streaming.Capability] {
		return nil
	}
	return func(ctx context.Context, absolute string) (*streaming.Spec, error) {
		return nativeapi.ReadStreamingQuery(ctx, s.connection.DB, s.connection.Database(), absolute)
	}
}

// workload reads the database's resource pools and classifiers, or is nil
// with the reason they are not read: a dev realm holds none of its own, and a
// target without resource pools lists none.
func (s *session) workload() (func(context.Context) (nativeapi.HeldWorkload, error), string) {
	switch {
	case s.realm:
		return nil, "A dev realm holds no resource pools or classifiers of its own: they belong to the whole database"
	case !s.capabilities[workload.CapabilityResourcePools]:
		return nil, "The target does not keep resource pools, so its empty system views establish no absence; " + workload.FlagHint
	}
	return func(ctx context.Context) (nativeapi.HeldWorkload, error) {
		return nativeapi.ReadWorkload(ctx, s.connection.DB, s.database)
	}, ""
}

// columnTables reads a column table's monitoring description with the
// session's credential, or is nil when the address names no monitoring
// endpoint, so that inspection refuses a column table by naming the parameter.
func (s *session) columnTables() inspect.ColumnTables {
	if !s.connection.HasMonitoring() {
		return nil
	}
	return func(ctx context.Context, absolute string) ([]byte, error) {
		return s.connection.ReadMonitoring(ctx, "column table", columnstore.Page, url.Values{"path": {absolute}, "database": {s.connection.Database()}})
	}
}

// NormalizeValues validates captured YDB schema models offline. Results
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
		var value *protocol.Envelope
		var err error
		format := input.GetValue().GetKind() + "/observed-json-v1"
		switch input.GetRepresentation() {
		case protocol.ValueInput_OPERATION:
			normalized, operationErr := normalizeOperation(input)
			if operationErr != nil {
				return nil, status.Error(codes.InvalidArgument, operationErr.Error())
			}
			result.Values = append(result.Values, normalized)
			continue
		case protocol.ValueInput_CHANGE:
			normalize := schema.NormalizeCoordinationChange
			switch input.GetValue().GetKind() {
			case schema.TablePropertiesChange:
				normalize = schema.NormalizeTablePropertiesChange
			case schema.ColumnStorageChange:
				normalize = schema.NormalizeColumnStorageChange
			case schema.TopicChange:
				normalize = schema.NormalizeTopicChange
			case schema.ExternalSourceChange, schema.ExternalTableChange:
				normalize = schema.NormalizeExternalChange
			case schema.ResourcePoolChange, schema.ResourcePoolClassifierChange:
				normalize = schema.NormalizeWorkloadChange
			case schema.StreamingQueryChange:
				normalize = schema.NormalizeStreamingQueryChange
			}
			normalized, changeErr := normalize(input)
			if changeErr != nil {
				return nil, status.Error(codes.InvalidArgument, changeErr.Error())
			}
			result.Values = append(result.Values, normalized)
			continue
		case protocol.ValueInput_OBSERVED:
			value, err = schema.Normalize(input.GetValue())
		case protocol.ValueInput_DESIRED:
			value, err = schema.NormalizeDesired(input.GetValue())
			format = input.GetValue().GetKind() + "/desired-json-v1"
		default:
			return nil, status.Error(codes.InvalidArgument, "unsupported YDB model representation")
		}
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		required, err := schema.RequiredObjects(value)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		result.Values = append(result.Values, &protocol.NormalizedValue{Id: input.GetId(), Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
			CanonicalFormat: format, CanonicalProjection: bytes.Clone(value.Payload), RequiredObjects: required})
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}
