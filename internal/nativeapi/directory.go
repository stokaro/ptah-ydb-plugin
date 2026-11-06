package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"path"

	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// ErrDirectoryNotVisible marks a directory listing YDB answered with
// SCHEME_ERROR: the path does not exist, or the session may not see it.
var ErrDirectoryNotVisible = errors.New("YDB directory does not exist or is not visible")

// NestedDatabaseError reports a path at or below the root of another database
// that sits below the session's database, such as /local/db2 below /local.
// YDB lists that root in its parent as an entry of type DATABASE, lists the
// root itself to the session, and answers a path below it with SCHEME_ERROR.
// What the nested database holds is its own schema: a session of the outer
// database cannot drop a table there (`Cannot find table`), and a table it
// creates there lands in the nested database's scheme.
type NestedDatabaseError struct {
	// Path is the absolute path whose listing was asked for, and Root the
	// nested database's root, relative to the session's database.
	Path, Root string
}

func (e *NestedDatabaseError) Error() string {
	return fmt.Sprintf("list YDB directory %s: %s is another YDB database, nested in this one", e.Path, e.Root)
}

// DirectoryLister lists the directory at an absolute path. It answers a path
// YDB does not show with an error that wraps [ErrDirectoryNotVisible].
type DirectoryLister func(ctx context.Context, absolute string) (*Ydb_Scheme.ListDirectoryResult, error)

// SchemeLister lists directories through client. A transport failure and a
// status other than SUCCESS and SCHEME_ERROR are errors that carry no server
// text.
func SchemeLister(client Ydb_Scheme_V1.SchemeServiceClient) DirectoryLister {
	return func(ctx context.Context, absolute string) (*Ydb_Scheme.ListDirectoryResult, error) {
		response, err := client.ListDirectory(ctx, &Ydb_Scheme.ListDirectoryRequest{Path: absolute})
		if err != nil {
			return nil, fmt.Errorf("list YDB directory %s: transport failed", absolute)
		}
		if operation := response.GetOperation(); operation.GetReady() && operation.GetStatus() == Ydb.StatusIds_SCHEME_ERROR {
			return nil, fmt.Errorf("list YDB directory %s: %w", absolute, ErrDirectoryNotVisible)
		}
		var result Ydb_Scheme.ListDirectoryResult
		if err := Operation(response.GetOperation(), &result); err != nil {
			return nil, fmt.Errorf("list YDB directory %s: %w", absolute, err)
		}
		return &result, nil
	}
}

// ListDirectory lists the directory relative names under database, the
// session's database or dev realm. A directory YDB does not show reads as an
// empty directory when the listing of its parent, a directory proved the same
// way up to database, does not name it. That is the evidence a walk from
// database takes for every entry a listing leaves out, and YDB creates the
// missing directories of every object it creates, so it lets a read before a
// write find no occupant where the first object of a new directory goes.
//
// A parent that names the directory proves it exists, so the refusal to list
// it stands, and a parent that is not a directory refuses the read: nothing can
// be created below it. database itself is never read as empty. Inspection and
// every native write's precondition read through this one rule.
//
// A path at or below a nested database is refused with a
// [NestedDatabaseError]: the root because YDB lists it as a DATABASE, a path
// below it because the root is its nearest listed ancestor. Read as a
// directory, the root would let a write's precondition pass and the write
// create its object in the nested database.
func ListDirectory(ctx context.Context, list DirectoryLister, database, relative string) (*Ydb_Scheme.ListDirectoryResult, error) {
	absolute := path.Join(database, relative)
	listed, err := list(ctx, absolute)
	if relative != "" && err == nil && listed.GetSelf().GetType() == Ydb_Scheme.Entry_DATABASE {
		return nil, &NestedDatabaseError{Path: absolute, Root: relative}
	}
	if relative == "" || !errors.Is(err, ErrDirectoryNotVisible) {
		return listed, err
	}
	parent := path.Dir(relative)
	if parent == "." {
		parent = ""
	}
	above, parentErr := ListDirectory(ctx, list, database, parent)
	if nested, ok := errors.AsType[*NestedDatabaseError](parentErr); ok {
		return nil, &NestedDatabaseError{Path: absolute, Root: nested.Root}
	}
	if parentErr != nil {
		return nil, parentErr
	}
	kind := above.GetSelf().GetType()
	if above.GetSelf().GetName() != path.Base(path.Join(database, parent)) ||
		(kind != Ydb_Scheme.Entry_DIRECTORY && (parent != "" || kind != Ydb_Scheme.Entry_DATABASE)) {
		return nil, fmt.Errorf("list YDB directory %s: its parent is not a directory", absolute)
	}
	name := path.Base(relative)
	for _, child := range above.GetChildren() {
		if child.GetName() == name {
			return nil, fmt.Errorf("list YDB directory %s: its parent lists it, and YDB does not show it to the session: %w", absolute, ErrDirectoryNotVisible)
		}
	}
	return &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: name, Type: Ydb_Scheme.Entry_DIRECTORY}}, nil
}
