package backend

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"path"
	"slices"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/columnstore"
	"github.com/stokaro/ptah-ydb-plugin/internal/inspect"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Inspect serializes inspection with close on this session only. A closed
// handle cannot reopen a connection or fall back to an embedded engine.
func (p *Provider) Inspect(ctx context.Context, request *protocol.InspectRequest) (*protocol.Schema, error) {
	if !slices.Contains(provider.HostFeatures(ctx), provider.ObservedColumnsFeature) {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB inspection requires schema.observed-columns")
	}
	p.mu.Lock()
	session := p.sessions[request.GetSessionId()]
	p.mu.Unlock()
	if session == nil {
		return nil, runtime.ErrorWithReason(codes.NotFound, "unknown YDB session")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "YDB session is closed")
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
	common.ContextInventory = slices.Contains(negotiatedFeatures(ctx), provider.PlanningContextInventoryFeature)
	common.Indexes = slices.Contains(negotiatedFeatures(ctx), provider.ObservedIndexesFeature)
	common.IndexFacets = common.Indexes && slices.Contains(negotiatedFeatures(ctx), provider.ObservedIndexFacetsFeature)
	common.PrimaryKeys = slices.Contains(negotiatedFeatures(ctx), provider.ObservedPrimaryKeysFeature)
	common.IndexMethods = slices.Contains(negotiatedFeatures(ctx), provider.IndexMethodsFeature)
	common.UnreadTables = slices.Contains(negotiatedFeatures(ctx), provider.UnreadTablesFeature)
	result, err := inspect.ReadObjects(ctx, session.connection.Driver, session.columnTables(), session.database, request.GetSchemas(), common, session.streaming(), session.leftOut()...)
	if err != nil {
		return nil, inspectError(ctx, err)
	}
	read, unread := session.workload()
	databaseKinds := slices.Contains(negotiatedFeatures(ctx), provider.DatabaseKindsFeature)
	if err := inspect.Workload(ctx, result, session.database, request.GetSchemas(), databaseKinds, read, unread); err != nil {
		return nil, inspectError(ctx, err)
	}
	if carriesCommon(provider.HostFeatures(ctx)) {
		if err := session.inspectAccess(ctx, result); err != nil {
			return nil, err
		}
	}
	if !session.capabilities["secrets"] {
		inspect.UnavailableSecrets(result)
	}
	result.TargetFeatures = negotiatedFeatures(ctx)
	if slices.Contains(result.TargetFeatures, provider.ValueContextFeature) {
		bindInspectionContext(result, &protocol.DatabaseContext{Database: session.database, SourceDatabase: session.connection.Database()})
	}
	if value, feature := schema.UnofferedFeature(provider.HostFeatures(ctx), inspectedValues(result)...); feature != "" {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, fmt.Sprintf("YDB inspection of %s %q requires %s, which the host did not offer", value.GetKind(), path.Join(value.GetOwner().GetSchema(), value.GetOwner().GetName()), feature))
	}
	// The reply is checked against the description the host holds: one that
	// did not list the common wire declares no common object.
	if _, err := provider.InspectionValues(provider.NegotiatedDescription(described(), provider.HostFeatures(ctx)), request, result); err != nil {
		return nil, authored.Status(codes.Internal, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}

// inspectAccess reads the owners, grants and principals of what the snapshot
// holds when the host takes common access, and otherwise records role and
// grant knowledge as uninspected. Its error is the call's status.
func (s *session) inspectAccess(ctx context.Context, result *protocol.Schema) error {
	if !slices.Contains(provider.HostFeatures(ctx), provider.CommonAccessFeature) {
		for _, kind := range []protocol.CommonObjectSupport_Kind{protocol.CommonObjectSupport_ROLE, protocol.CommonObjectSupport_GRANT} {
			result.CommonObjects.Coverage = append(result.CommonObjects.Coverage, &protocol.CommonObjectCoverage{Kind: kind,
				Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "Access inspection requires schema.common-access"}})
		}
		return nil
	}
	principals := func(ctx context.Context) (nativeapi.Principals, error) {
		return nativeapi.ReadPrincipals(ctx, s.connection.DB, s.connection.Database())
	}
	if err := inspect.Access(ctx, result, s.database, s.scope(), nativeapi.AccessReader(s.connection.Driver), principals); err != nil {
		return inspectError(ctx, err)
	}
	if len(result.GetCommonObjects().GetRolesOutOfScope()) > 0 && slices.Contains(negotiatedFeatures(ctx), provider.RoleScopeReasonsFeature) {
		result.CommonObjects.RolesOutOfScopeReason = inspect.RolesOutOfScopeReason
	}
	return nil
}

// inspectError is the status of a failed inspection: the context's when the
// context has ended, and FailedPrecondition otherwise.
func inspectError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return authored.Status(codes.FailedPrecondition, err)
}

// inspectedValues returns every value a snapshot carries: native objects and
// the facets of tables and columns.
func inspectedValues(snapshot *protocol.Schema) []*protocol.Envelope {
	values := slices.Clone(snapshot.GetNativeObjects())
	for _, table := range snapshot.GetTables() {
		values = append(values, table.GetFacets()...)
		for _, column := range table.GetColumns() {
			values = append(values, column.GetFacets()...)
		}
	}
	return values
}

// scope is what the session's reads are confined to: its dev realm, or the
// whole database.
func (s *session) scope() inspect.Scope {
	if s.realm {
		return inspect.RealmScope
	}
	return inspect.DatabaseScope
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
	return nativeapi.RoutedRead(func(ctx context.Context) (nativeapi.HeldWorkload, error) {
		held, err := nativeapi.ReadWorkload(ctx, s.connection.DB, s.database)
		if err != nil {
			return held, err
		}
		list := nativeapi.SchemeLister(Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(s.connection.Driver)))
		held.Listed, err = nativeapi.ListedPools(ctx, list, s.database)
		return held, err
	}), ""
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
	if err := provider.ValidateValueInputs(described(), request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
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
				return nil, authored.Status(codes.InvalidArgument, operationErr)
			}
			result.Values = append(result.Values, normalized)
			continue
		case protocol.ValueInput_CHANGE:
			normalized, changeErr := changeNormalizer(input.GetValue().GetKind())(input)
			if changeErr != nil {
				return nil, authored.Status(codes.InvalidArgument, changeErr)
			}
			result.Values = append(result.Values, normalized)
			continue
		case protocol.ValueInput_OBSERVED:
			value, err = schema.Normalize(input.GetValue())
		case protocol.ValueInput_DESIRED:
			value, err = schema.NormalizeDesired(input.GetValue())
			format = input.GetValue().GetKind() + "/desired-json-v1"
		default:
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, "unsupported YDB model representation")
		}
		if err != nil {
			return nil, authored.Status(codes.InvalidArgument, err)
		}
		required, pending, err := schema.RequiredObjects(value)
		if err != nil {
			return nil, authored.Status(codes.InvalidArgument, err)
		}
		result.Values = append(result.Values, &protocol.NormalizedValue{Id: input.GetId(), Value: value, NormalizedOwner: proto.CloneOf(value.Owner),
			CanonicalFormat: format, CanonicalProjection: bytes.Clone(value.Payload), RequiredObjects: required, RequiresDatabaseContext: pending})
	}
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	return result, nil
}

// changeNormalizer returns the normalizer of a change of kind; a kind of no
// other change is a coordination node's.
func changeNormalizer(kind string) func(*protocol.ValueInput) (*protocol.NormalizedValue, error) {
	switch kind {
	case schema.TablePropertiesChange:
		return schema.NormalizeTablePropertiesChange
	case schema.ColumnStorageChange:
		return schema.NormalizeColumnStorageChange
	case schema.SecretChange:
		return schema.NormalizeSecretChange
	case schema.TopicChange:
		return schema.NormalizeTopicChange
	case schema.ExternalSourceChange, schema.ExternalTableChange:
		return schema.NormalizeExternalChange
	case schema.ResourcePoolChange, schema.ResourcePoolClassifierChange:
		return schema.NormalizeWorkloadChange
	case schema.StreamingQueryChange:
		return schema.NormalizeStreamingQueryChange
	case schema.ReplicationChange:
		return schema.NormalizeReplicationChange
	case schema.TransferChange:
		return schema.NormalizeTransferChange
	default:
		return schema.NormalizeCoordinationChange
	}
}
