package backend

import (
	"context"
	"errors"
	"path"

	"github.com/stokaro/ptah-ydb-plugin/internal/devclean"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// devSession locks the session a dev cleanup names for the request. A
// cleanup changes the schema, which YDB refuses inside a transaction, so a
// session with an active transaction is refused.
func (p *Provider) devSession(ctx context.Context, id string) (*session, error) {
	s, err := p.lookup(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "YDB session is closed")
	case s.active != "":
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "YDB session has an active transaction")
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, status.FromContextError(err).Err()
	}
	return s, nil
}

// devEntries walks what a cleanup of the schemas would remove or stop at:
// the whole root the session treats as its database when schemas is empty,
// and otherwise what each named directory holds. A named directory that does
// not exist holds nothing. The lock node the session keeps its locks on, the
// directory of the dev realms at the root of a database, and the server's own
// objects are left out.
func (s *session) devEntries(ctx context.Context, cleaner devclean.Native, schemas []string) ([]devclean.Entry, error) {
	tree := devclean.Tree{Root: s.database, Database: s.connection.Database(), KeepLocks: true}
	if len(schemas) == 0 {
		return devclean.Walk(ctx, cleaner, tree, "")
	}
	var entries []devclean.Entry
	for _, schema := range schemas {
		dir := path.Clean(schema)
		if dir == "." || dir == "/" {
			dir = ""
		}
		if path.IsAbs(dir) || dir == ".." || len(dir) >= 3 && dir[:3] == "../" {
			return nil, status.Error(codes.InvalidArgument, "a dev cleanup schema is a directory below the database")
		}
		found, err := devclean.Walk(ctx, cleaner, tree, dir)
		if errors.Is(err, devclean.ErrScheme) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, found...)
	}
	return entries, nil
}

// devObjects turns walked entries into the objects of a reply. An object the
// cleanup has no statement for carries the reason it stops the cleanup.
func devObjects(entries []devclean.Entry) []*protocol.DevObject {
	objects := make([]*protocol.DevObject, 0, len(entries))
	for _, entry := range entries {
		object := &protocol.DevObject{Object: &protocol.Reference{Engine: "ydb", Kind: entry.Object.Kind, Schema: entry.Object.Schema, Name: entry.Object.Name}}
		if !entry.Droppable() {
			object.Refusal = devclean.Refusal(entry.Object)
		}
		objects = append(objects, object)
	}
	return objects
}

// ListDevObjects lists every object a cleanup of the request's scope would
// remove or stop at, in the order of a depth-first walk with each directory's
// entries in name order, a directory after its contents.
func (p *Provider) ListDevObjects(ctx context.Context, request *protocol.ListDevObjectsRequest) (*protocol.DevObjects, error) {
	s, err := p.devSession(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	entries, err := s.devEntries(ctx, devclean.NewNative(s.connection, nil), request.GetSchemas())
	if err != nil {
		return nil, devError(ctx, err)
	}
	return &protocol.DevObjects{Complete: true, Objects: devObjects(entries)}, nil
}

// CleanDevDatabase empties the request's scope. It drops every transfer and
// streaming query, then every async replication, then external tables and
// column tables, then external data sources, then the other objects, then
// secrets, and removes the directories, deepest first. An object there is no
// statement for refuses the cleanup before anything is dropped. A cleanup
// that stopped after its first step is UNKNOWN.
func (p *Provider) CleanDevDatabase(ctx context.Context, request *protocol.CleanDevRequest) (*protocol.DevCleanup, error) {
	s, err := p.devSession(ctx, request.GetSessionId())
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	result := &protocol.DevCleanup{RequestId: request.GetRequestId()}
	guarded, stop, err := s.guard(ctx)
	if err != nil {
		result.Outcome = protocol.DevCleanup_UNKNOWN
		result.Message = "the session's locks are no longer held; nothing was dropped"
		return result, nil
	}
	defer stop()
	ctx = guarded
	cleaner := devclean.NewNative(s.connection, s.verifyLocks)
	entries, err := s.devEntries(ctx, cleaner, request.GetSchemas())
	if err != nil {
		return nil, devError(ctx, err)
	}
	if blocking := devclean.Blocking(entries); len(blocking) > 0 {
		result.Outcome = protocol.DevCleanup_REFUSED
		for _, object := range devObjects(entries) {
			if object.GetRefusal() != "" {
				result.Blocking = append(result.Blocking, object)
			}
		}
		return result, nil
	}
	started, err := devclean.Run(ctx, cleaner, cleaner, entries)
	switch {
	case err == nil:
		result.Outcome = protocol.DevCleanup_CLEANED
	case !started:
		return nil, devError(ctx, err)
	default:
		result.Outcome = protocol.DevCleanup_UNKNOWN
		result.Message = "the cleanup stopped part way; the dev database is not clean"
	}
	return result, nil
}

// devError reports a walk or a cleanup that could not run.
func devError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr).Err()
	}
	return status.Error(codes.Unavailable, err.Error())
}
