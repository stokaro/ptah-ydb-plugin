package backend

import (
	"context"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/authored"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/devdatabase"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// ResolveDatabaseContext interprets the address without opening a connection
// or resolving credentials. A dev realm retains the authored database root,
// and a dev database spec names local-ydb's (see addressContext).
func (*Provider) ResolveDatabaseContext(ctx context.Context, request *protocol.DatabaseContextRequest) (*protocol.DatabaseContext, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidateDatabaseContextRequest(described(), request); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	result, err := addressContext(request.GetAddress())
	if err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	if err := schema.ValidateDatabaseContext(result); err != nil {
		return nil, authored.Status(codes.InvalidArgument, err)
	}
	return result, nil
}

// addressContext is the database context address names: that of a YDB
// address, or that of a dev database spec, `docker://ydb[/<tag>[/local]]` or
// `docker+ydb://...`, whose database is the one local-ydb serves. A file to
// file comparison names its dev database by the spec, before anything
// starts, and the host's embedded engine read the spec's root as /local.
func addressContext(raw string) (*protocol.DatabaseContext, error) {
	if scheme, _, found := strings.Cut(raw, "://"); found && (scheme == "docker" || scheme == "docker+ydb") {
		if _, err := devdatabase.ParseSpec(raw); err != nil {
			return nil, authored.New("the dev database spec names no YDB database: " + err.Error())
		}
		database := "/" + devdatabase.Database
		return &protocol.DatabaseContext{Database: database, SourceDatabase: database}, nil
	}
	address, err := connection.Parse(raw)
	if err != nil {
		return nil, err
	}
	return databaseContext(address), nil
}

func databaseContext(address connection.Address) *protocol.DatabaseContext {
	return &protocol.DatabaseContext{Database: address.Root(), SourceDatabase: address.Database()}
}

func bindInspectionContext(snapshot *protocol.Schema, bound *protocol.DatabaseContext) {
	bind := func(values []*protocol.Envelope) {
		for _, value := range values {
			value.Context = proto.CloneOf(bound)
		}
	}
	bind(snapshot.NativeObjects)
	for _, table := range snapshot.Tables {
		bind(table.Facets)
		for _, column := range table.Columns {
			bind(column.Facets)
		}
	}
	for _, view := range snapshot.GetCommonObjects().GetViews() {
		bind(view.Facets)
	}
}

var _ provider.DatabaseContextResolver = (*Provider)(nil)
