//go:build integration

package integration_test

import (
	"context"
	"path"
	"regexp"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// boundTable creates the table at absolute with the changefeed updates, whose
// replication binding names the replica at replica, as the controller of an
// async replication writes it on the table the replication reads. YDB takes
// the attribute from a client, measured on 25.1.4.7 and 26.2.1.14, so the
// stream of a replication that lives on another server, or in a nested
// database, needs no second server to stand for it.
func boundTable(c *qt.C, opened *connection.Connection, absolute, replica string) {
	c.Helper()
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` (id Int64 NOT NULL, PRIMARY KEY (id))"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, opened.DB, "DROP TABLE `"+absolute+"`")
	})
	client := Ydb_Table_V1.NewTableServiceClient(ydb.GRPCConn(opened.Driver))
	session, err := client.CreateSession(c.Context(), &Ydb_Table.CreateSessionRequest{})
	c.Assert(err, qt.IsNil)
	var created Ydb_Table.CreateSessionResult
	c.Assert(session.GetOperation().GetResult().UnmarshalTo(&created), qt.IsNil)
	c.Cleanup(func() {
		_, _ = client.DeleteSession(context.Background(), &Ydb_Table.DeleteSessionRequest{SessionId: created.GetSessionId()})
	})
	altered, err := client.AlterTable(c.Context(), &Ydb_Table.AlterTableRequest{SessionId: created.GetSessionId(), Path: absolute,
		AddChangefeeds: []*Ydb_Table.Changefeed{{Name: "updates", Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES, Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON,
			Attributes: map[string]string{"__async_replication": `{"path":"` + replica + `","id":"1","supports_topic_autopartitioning":false}`}}}})
	c.Assert(err, qt.IsNil)
	c.Assert(altered.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("%v", altered.GetOperation().GetIssues()))
}

// A table whose stream an async replication controls is not dropped where the
// binding names a replica below this database's root that this database does
// not hold: the replication is one of a database of the same name on another
// server, which no plan of this one drops, and YDB drops the table and leaves
// that replication failing. The host's embedded engine refused the drop of
// any table a binding names (`a replication-managed changefeed depends on this
// table`, measured with this fixture on 26.2.1.14). Without the read's reason
// the plan drops it, since no replication of this database reads it.
func TestHostRefusesToDropATableAReplicationOnAnotherServerReads_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	replica := path.Join(database, directory, "elsewhere", "rep")
	boundTable(c, opened, path.Join(database, directory, "src"), replica)

	refused, err := hostApplySource(c, directory, "yaml", "tables: {}\n")

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Matches, `(?s).*table "`+regexp.QuoteMeta(path.Join(directory, "src"))+`": changefeed "updates" is a replication-managed stream that `+
		`depends on this table; its replication's controller must release it before the table is dropped, and the replication, which writes `+
		regexp.QuoteMeta(replica)+`, lives in another database \(this database holds no table at that path, so a database of the same name `+
		`on another server holds the replica\); drop it there first, or keep the table.*`)
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"src"})
}

// So is a table whose stream a replication of a database nested in this one
// controls: the binding names the replica below this database's root, inside
// the nested database, which a plan of this database cannot reach.
func TestHostRefusesToDropATableAReplicationOfANestedDatabaseReads_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	_, other := otherDatabase(c)
	nested := other.Driver.Name()
	opened, directory, database := lockFixture(c)
	replica := path.Join(nested, directory, "rep")
	boundTable(c, opened, path.Join(database, directory, "src"), replica)

	refused, err := hostApplySource(c, directory, "yaml", "tables: {}\n")

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", refused))
	c.Assert(refused, qt.Matches, `(?s).*table "`+regexp.QuoteMeta(path.Join(directory, "src"))+`": changefeed "updates" is a replication-managed stream that `+
		`depends on this table; its replication's controller must release it before the table is dropped, and the replication, which writes `+
		regexp.QuoteMeta(replica)+`, lives in another database \(the replica lies in the database `+regexp.QuoteMeta(nested)+
		`, which is nested in this one\); drop it there first, or keep the table.*`)
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.DeepEquals, []string{"src"})
}
