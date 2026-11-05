package tablecreate

import (
	"path"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// alteredAccess is the access reason of a change to a table that exists: YDB
// keeps a table's access control entries through every change of its columns,
// families, settings, TTL or indexes, and has no column-level permissions.
const alteredAccess = "Altering a table preserves its access metadata"

// createdAccess is the access assessment of creating the table named name,
// the owner of its creation, which a rebuild names with the scratch table's
// name. A table the plan creates holds no row before it, so no role could
// read anything there, and it takes the permissions of its directory. A rebuild's
// scratch table is the exception: the rows copied into it come from a table
// whose own permissions it does not take, which may have granted less than
// the directory does, so whether the rebuild widens access is not established.
func createdAccess(name string) (protocol.ChangeSemantics_Access, string) {
	if strings.HasPrefix(path.Base(name), rebuildPrefix) {
		return protocol.ChangeSemantics_ACCESS_UNKNOWN,
			"A rebuild's new table takes its directory's permissions, not those of the table whose rows it receives"
	}
	return protocol.ChangeSemantics_UNCHANGED, "The table is created by this plan, so no role could read it before; it takes its directory's permissions"
}
