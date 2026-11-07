package replication

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// SecretPaths are the scheme secrets connection names by path, token first,
// each relative to root, the absolute path of the database the replication or
// transfer runs in. YDB looks such a secret up when it creates the object or
// changes its connection (`secret ... not found`), so whatever creates the
// secret has to run first, and dropping it leaves the object naming a secret
// that is gone. YDB keeps the path absolute, so an absolute path under root
// names the secret its relative path names.
//
// A path written absolute where root is not known, and one that cannot name a
// secret, name no secret Ptah manages and are left out. A path outside root
// is refused: no statement of the object can read it. Each secret appears
// once.
func SecretPaths(root string, connection Connection) ([]ydbdecl.Path, error) {
	var paths []ydbdecl.Path
	for _, written := range []string{connection.TokenSecretPath, connection.PasswordSecretPath} {
		written = strings.TrimSpace(written)
		if written == "" {
			continue
		}
		relative := written
		if strings.HasPrefix(written, "/") {
			database := strings.Trim(root, "/")
			if database == "" {
				continue
			}
			var under bool
			if relative, under = strings.CutPrefix(path.Clean(written), "/"+database+"/"); !under {
				return nil, fmt.Errorf("secret path %q is outside the database /%s, so no statement of it can read the secret", written, database)
			}
		}
		ref, err := ydbdecl.ParseExternalPath(relative)
		if err != nil || secrets.ValidateIdentity(ref.Schema, ref.Name) != nil || slices.Contains(paths, ref) {
			continue
		}
		paths = append(paths, ref)
	}
	return paths, nil
}
