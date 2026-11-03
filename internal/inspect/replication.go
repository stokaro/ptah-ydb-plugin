package inspect

import (
	"errors"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
)

// unreadReplication says why a listed async replication or transfer is left
// uninspected, or "" for a read that succeeded or failed outright. A
// cluster that does not serve the replication API, and a description the
// provider cannot model, leave the object unmanaged rather than fail the
// snapshot: a comparison then decides nothing about it, and a plan neither
// creates nor drops anything at its path.
func unreadReplication(err error) string {
	switch {
	case errors.Is(err, nativeapi.ErrReplicationServiceUnavailable):
		return "The cluster does not serve the replication API (YDB_GRPC_SERVICES lacks replication), so the provider leaves the object unmanaged"
	case errors.Is(err, nativeapi.ErrUnreadable):
		return "The provider leaves the object unmanaged: " + err.Error()
	default:
		return ""
	}
}
