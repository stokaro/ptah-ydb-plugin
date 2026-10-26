// Package externalpath resolves YDB external-object dependencies.
package externalpath

import (
	"errors"
	"fmt"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"maps"
	"path"
	"slices"
	"strings"
)

const secretPathSuffix = "_SECRET_PATH"

// ErrDatabaseContextRequired means an absolute reference cannot yet be bound.
var ErrDatabaseContextRequired = errors.New("an absolute YDB dependency requires database context")

// IsSecretOption identifies the shared spelling used by dependency capture,
// comparison, conversion, and rendering. Those paths must recognize one set.
func IsSecretOption(name string) bool {
	return strings.HasSuffix(strings.ToUpper(name), secretPathSuffix)
}

// Rebase preserves an absolute dependency's meaning across representation roots.
// Relative references retain their spelling.
func Rebase(written, from, to string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(written), "/") {
		return written, nil
	}
	ref, err := SourcePath(from, written)
	if err != nil {
		return "", err
	}
	if to == "" {
		return "", ErrDatabaseContextRequired
	}
	return to + "/" + ydbdecl.ExternalDisplay(ref.Schema, ref.Name), nil
}

// RelativePath is a path an option or a description holds, relative to root,
// the database a read describes: an absolute path under root loses root, and
// any other path is kept as it is.
func RelativePath(value, root string) string {
	if strings.Trim(root, "/") == "" {
		return value
	}
	root = "/" + strings.Trim(root, "/")
	if rest, under := strings.CutPrefix(value, root+"/"); under && strings.HasPrefix(value, "/") {
		return path.Clean(rest)
	}
	return value
}

// SourcePath reads the path of a data source an external table names, as
// a directory and a name relative to root: relative as written, or absolute
// under root. It refuses a path outside root or one that names no object.
func SourcePath(root, written string) (ydbdecl.Path, error) {
	trimmed := strings.TrimSpace(written)
	if strings.HasPrefix(trimmed, "/") {
		if strings.Trim(root, "/") == "" {
			return ydbdecl.Path{}, ErrDatabaseContextRequired
		}
		relative, under := strings.CutPrefix(path.Clean(trimmed), "/"+strings.Trim(root, "/")+"/")
		if !under {
			return ydbdecl.Path{}, fmt.Errorf("external data source path is outside the database context")
		}
		trimmed = relative
	}
	return ydbdecl.ParseExternalPath(trimmed)
}

// SecretPaths are the secrets the options of a data source name by their
// path, relative to root, in option order. A path outside the database is
// refused with the host's words: the data source cannot read it.
func SecretPaths(options map[string]string, root string) ([]ydbdecl.Path, error) {
	var paths []ydbdecl.Path
	for _, option := range slices.Sorted(maps.Keys(options)) {
		written := strings.TrimSpace(options[option])
		if !IsSecretOption(option) || written == "" {
			continue
		}
		relative := written
		if strings.HasPrefix(written, "/") {
			if root == "" {
				return nil, ErrDatabaseContextRequired
			}
			relative = RelativePath(written, root)
			if strings.HasPrefix(relative, "/") {
				return nil, fmt.Errorf("option %s names secret %q, which is outside the database /%s, so the data source cannot read it",
					option, written, strings.Trim(root, "/"))
			}
		}
		ref, err := ydbdecl.ParseExternalPath(relative)
		if err != nil {
			return nil, fmt.Errorf("option %s names an invalid secret path: %w", option, err)
		}
		if err := secrets.ValidateIdentity(ref.Schema, ref.Name); err != nil {
			return nil, fmt.Errorf("option %s names an invalid secret path: %w", option, err)
		}
		if slices.Contains(paths, ref) {
			continue
		}
		paths = append(paths, ref)
	}
	return paths, nil
}
