package inspect

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// foreignReplication says why the async replication that controls feed lives
// in another database although the replica its binding names lies below
// root, or returns "". The binding names the replica by its absolute path in
// the replication's own database, so a path below root does not by itself
// put the replication here: the path may lie in a database nested in this
// one, such as /local/db2 below /local, or name a table of a database of the
// same name on another server. The read lists the path from root down, and a
// nested database on the way, or no table at its end, says which.
//
// The planner of tables refuses to drop a table whose stream such a
// replication controls, as it refuses one whose replica lies outside root: no
// plan of this database drops that replication, and YDB drops the table and
// leaves the replication failing. Without this reason the drop is planned,
// because the planner of replications finds no replication here that reads
// the table.
func (s *source) foreignReplication(ctx context.Context, root string, feed *native.ChangefeedDescription) (string, error) {
	replica := changefeeds.ReplicaPath(feed.GetAttributes())
	relative, below := strings.CutPrefix(replica, strings.TrimRight(root, "/")+"/")
	if replica == "" || !below {
		return "", nil
	}
	absent := "this database holds no table at that path, so a database of the same name on another server holds the replica"
	directory := root
	names := strings.Split(relative, "/")
	for index, name := range names {
		listing, err := s.directory(ctx, directory)
		if err != nil {
			return "", err
		}
		var entry *Ydb_Scheme.Entry
		for _, child := range listing.GetChildren() {
			if child.GetName() == name {
				entry = child
			}
		}
		last := index == len(names)-1
		switch {
		case entry == nil:
			return absent, nil
		case entry.GetType() == Ydb_Scheme.Entry_DATABASE:
			return fmt.Sprintf("the replica lies in the database %s, which is nested in this one", path.Join(directory, name)), nil
		case last && (entry.GetType() == Ydb_Scheme.Entry_TABLE || entry.GetType() == Ydb_Scheme.Entry_COLUMN_TABLE):
			return "", nil
		case last || entry.GetType() != Ydb_Scheme.Entry_DIRECTORY:
			return absent, nil
		}
		directory = path.Join(directory, name)
	}
	return absent, nil
}
