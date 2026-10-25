package backend

import (
	"context"
	"maps"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/commonobject"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// PrepareCommonObjects returns a declaration of views and comments in YDB's
// spelling, offline: each view's query as the server stores it, so the host
// compares a declared view with a read one and a view applied once is not
// planned again.
func (*Provider) PrepareCommonObjects(ctx context.Context, request *protocol.PrepareCommonObjectsRequest) (*protocol.PreparedCommonObjects, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	description := Description()
	if err := provider.ValidatePrepareCommonObjectsRequest(description, request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	result := &protocol.PreparedCommonObjects{Complete: true, Prepared: commonobject.Prepare(request.GetDeclared())}
	if err := provider.ValidatePreparedCommonObjects(description, request, result); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return result, nil
}

// carriesCommon reports a target profile that negotiated the common wire,
// on which views and comments are the host's common objects.
func carriesCommon(features []string) bool {
	return slices.Contains(features, provider.CommonObjectsFeature)
}

// commonCapabilities adds the capability facts of the common objects to a
// session's on a host that negotiated the wire, and leaves them out for any
// other host, which never sees a common object.
func commonCapabilities(ctx context.Context, capabilities map[string]bool) map[string]bool {
	if !carriesCommon(provider.HostFeatures(ctx)) {
		return capabilities
	}
	result := maps.Clone(capabilities)
	maps.Copy(result, commonobject.Capabilities())
	return result
}

// withoutDeclaredComments returns tables with each declaration's comments
// left out where the target profile carries them on the common wire, after
// checking that YDB can hold them. The table services see the table alone.
func withoutDeclaredComments(features []string, declarations ...**protocol.DeclaredTable) error {
	if !carriesCommon(features) {
		return nil
	}
	for _, declaration := range declarations {
		if *declaration == nil {
			continue
		}
		if err := commonobject.TableCommentRefusal(*declaration); err != nil {
			return err
		}
		*declaration = commonobject.WithoutComments(*declaration)
	}
	return nil
}

// preparationWithoutComments is a copy of a preparation request whose tables
// declare no comment the common wire carries.
func preparationWithoutComments(request *protocol.PrepareTablesRequest) (*protocol.PrepareTablesRequest, error) {
	result := proto.CloneOf(request)
	for _, table := range result.GetTables() {
		if err := withoutDeclaredComments(result.GetTargetFeatures(), &table.Desired); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// creationsWithoutComments is a copy of a creation projection request whose
// tables declare no comment the common wire carries.
func creationsWithoutComments(request *protocol.ProjectTableCreationsRequest) (*protocol.ProjectTableCreationsRequest, error) {
	result := proto.CloneOf(request)
	for _, table := range result.GetTables() {
		if err := withoutDeclaredComments(result.GetTargetFeatures(), &table.Declaration); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// planningWithoutComments is a copy of a planning request whose desired
// tables declare no comment the common wire carries.
func planningWithoutComments(request *protocol.PlanFeaturesRequest) (*protocol.PlanFeaturesRequest, error) {
	result := proto.CloneOf(request)
	for _, table := range result.GetTables() {
		if err := withoutDeclaredComments(result.GetTargetFeatures(), &table.Desired); err != nil {
			return nil, err
		}
	}
	return result, nil
}
