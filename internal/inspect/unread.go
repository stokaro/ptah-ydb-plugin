package inspect

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// maxReasonBytes is the longest reason the contract lets a host show.
const maxReasonBytes = 512

// unreadReplicas names each replica table the read found that no async
// replication writes, as a table the read does not describe. YDB keeps such
// a table read-only and creates no table at its path, so a schema that
// declares one has to be refused at planning rather than fail when its
// CREATE TABLE runs. A replica a replication writes is not named: the
// replication's items name it, and the replication planner refuses a table
// declared there.
//
// targets are the paths every async replication of the database writes.
// When they are every replication's, a replica no target covers is one no replication writes any more, left behind by a
// replication dropped without CASCADE, and the reason says so as the host's
// embedded engine does. Without them, as when the cluster does not serve the
// replication API, the reason says the read does not know which replication
// writes the table.
func unreadReplicas(replicas []*protocol.Reference, targets replicationTargets) []*protocol.UnreadTable {
	var result []*protocol.UnreadTable
	for _, ref := range replicas {
		if replication.UnderTarget(replication.TablePath(ref.GetSchema(), ref.GetName()), targets.paths) {
			continue
		}
		result = append(result, &protocol.UnreadTable{Table: proto.CloneOf(ref), Diagnostic: &protocol.ProviderDiagnostic{Reason: targets.reason(ref)}})
	}
	return result
}

// replicationTargets are the paths the replications a read describes write,
// and whether the read described every replication of the database, so that
// a replica under none of them is written by none.
type replicationTargets struct {
	paths    []string
	complete bool
}

// snapshotTargets are the paths the replications a snapshot holds write.
func snapshotTargets(snapshot *protocol.Schema) ([]string, error) {
	var targets []string
	for _, value := range snapshot.GetNativeObjects() {
		if value.GetKind() != schema.ReplicationProperties {
			continue
		}
		spec, _, err := schema.ObservedReplication(value)
		if err != nil {
			return nil, err
		}
		targets = append(targets, replication.Targets(spec)...)
	}
	return targets, nil
}

// databaseTargets reads the paths every async replication of the database
// writes, for a filtered read whose own directories hold a replica: the
// replication that writes it may live in a directory the read was not asked
// for. It walks the directories as an unfiltered read does, leaving out the
// same ones, and reports complete as false when a replication is left
// uninspected.
func (s *source) databaseTargets(ctx context.Context, database string, leaveOut []string) (targets []string, complete bool, err error) {
	pending := []string{""}
	visited := make(map[string]bool)
	complete = true
	for len(pending) != 0 {
		directory := pending[0]
		pending = pending[1:]
		if visited[directory] || len(visited) >= 4096 {
			return nil, false, fmt.Errorf("invalid or excessive YDB replication traversal")
		}
		visited[directory] = true
		listed, err := nativeapi.ListDirectory(ctx, s.directory, database, directory)
		if err != nil {
			return nil, false, err
		}
		children := slices.Clone(listed.GetChildren())
		slices.SortFunc(children, func(a, b *Ydb_Scheme.Entry) int { return strings.Compare(a.GetName(), b.GetName()) })
		for _, entry := range children {
			name := entry.GetName()
			switch {
			case !component(name):
				return nil, false, fmt.Errorf("invalid YDB directory entry")
			case entry.GetType() == Ydb_Scheme.Entry_DIRECTORY && (strings.HasPrefix(name, ".") || directory == "" && slices.Contains(leaveOut, name)):
			case entry.GetType() == Ydb_Scheme.Entry_DIRECTORY:
				pending = append(pending, path.Join(directory, name))
			case entry.GetType() == Ydb_Scheme.Entry_REPLICATION:
				owner := &protocol.Reference{Engine: "ydb", Database: database, Schema: directory, Kind: schema.ReplicationProperties, Name: name}
				value, err := s.replications.Capture(ctx, database, owner)
				if unreadReplication(err) != "" {
					complete = false
					continue
				}
				if err != nil {
					return nil, false, err
				}
				spec, _, err := schema.ObservedReplication(value)
				if err != nil {
					return nil, false, err
				}
				targets = append(targets, replication.Targets(spec)...)
			}
		}
	}
	return targets, complete, nil
}

// reason says why the read leaves the replica table ref out, naming
// it as the host names a table, or as "it" where the name would make the
// reason longer than a host shows.
func (t replicationTargets) reason(ref *protocol.Reference) string {
	format := "%s is a replica table no replication of this database writes any more, which YDB keeps read-only for good " +
		"(`path is an async replica table`); drop it by hand with DROP TABLE, which YDB accepts, or remove it from the schema"
	if !t.complete {
		format = "%s is a replica table of an async replication this read cannot describe, and YDB keeps a replica table " +
			"read-only (`path is an async replica table`); remove it from the schema"
	}
	name := ref.GetName()
	if ref.GetSchema() != "" {
		name = ref.GetSchema() + "." + name
	}
	if reason := fmt.Sprintf(format, name); len(reason) <= maxReasonBytes {
		return reason
	}
	return fmt.Sprintf(format, "it")
}
