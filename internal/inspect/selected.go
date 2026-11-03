package inspect

import (
	"context"
	"errors"
	"path"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
)

// ErrNotVisible marks a directory listing YDB answered with SCHEME_ERROR: the
// path does not exist, or the session may not see it.
var ErrNotVisible = errors.New("YDB directory does not exist or is not visible")

// ListSelected lists the directory selected names, relative to database. A
// selected directory YDB does not show is read as empty when the listing of
// its parent, proved the same way up to the database root, does not name it.
// That is the evidence a walk from the root takes for every entry a listing
// leaves out, and it lets a filtered read create the first object of a new
// directory. A parent that names the directory proves it exists, so the
// refusal to list it stands. The database root is never read as empty.
func ListSelected(ctx context.Context, list func(context.Context, string) (*Ydb_Scheme.ListDirectoryResult, error), database, selected string) (*Ydb_Scheme.ListDirectoryResult, error) {
	listed, err := list(ctx, path.Join(database, selected))
	if selected == "" || !errors.Is(err, ErrNotVisible) {
		return listed, err
	}
	parent := path.Dir(selected)
	if parent == "." {
		parent = ""
	}
	above, parentErr := ListSelected(ctx, list, database, parent)
	if parentErr != nil {
		return nil, parentErr
	}
	name := path.Base(selected)
	for _, child := range above.GetChildren() {
		if child.GetName() == name {
			return nil, err
		}
	}
	return &Ydb_Scheme.ListDirectoryResult{Self: &Ydb_Scheme.Entry{Name: name, Type: Ydb_Scheme.Entry_DIRECTORY}}, nil
}
