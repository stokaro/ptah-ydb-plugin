//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// readTable is what an inspection reports of one table: the identity a
// comparison keys it by, its columns, and each index with its columns.
type readTable struct {
	Normalized       string
	Columns, Indexes []string
}

// tablesRead are the tables of snapshot named in owned, keyed by their
// identity, as directory|name.
func tablesRead(c *qt.C, snapshot *protocol.Schema, owned ...string) map[string]readTable {
	c.Helper()
	read := make(map[string]readTable)
	for _, table := range snapshot.GetTables() {
		identity := table.GetIdentity().GetSchema() + "|" + table.GetIdentity().GetName()
		if !slices.Contains(owned, identity) {
			continue
		}
		found := readTable{Normalized: table.GetNormalizedIdentity().GetSchema() + "|" + table.GetNormalizedIdentity().GetName()}
		for _, column := range table.GetColumns() {
			found.Columns = append(found.Columns, column.GetName())
		}
		for _, facet := range table.GetFacets() {
			if facet.GetKind() != schema.IndexProperties {
				continue
			}
			var indexes native.IndexObservations
			c.Assert(protojson.Unmarshal(facet.GetPayload(), &indexes), qt.IsNil)
			for _, index := range indexes.GetIndexes() {
				found.Indexes = append(found.Indexes, index.GetDescription().GetName()+" ON "+strings.Join(index.GetDescription().GetIndexColumns(), ","))
			}
		}
		read[identity] = found
	}
	return read
}

// A table at the database root whose name has a dot in it, d.b, and the
// table b in the directory d are two tables: YDB keeps the directory and the
// name apart, and so does the provider's read, which a key that joined the
// two with a dot would read as one. Each keeps its own columns and its own
// index, of one name, which YDB scopes to its table.
func TestRootTableAndDirectoryTableStayTwoSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root, nested := path.Join(database, directory+".b"), path.Join(database, directory, "b")
	for _, statement := range []string{
		"CREATE TABLE `" + root + "` (id Int64 NOT NULL, root_note Utf8, PRIMARY KEY (id), INDEX note_ix GLOBAL SYNC ON (root_note))",
		"CREATE TABLE `" + nested + "` (id Int64 NOT NULL, nested_note Utf8, PRIMARY KEY (id), INDEX note_ix GLOBAL SYNC ON (nested_note))",
	} {
		c.Assert(execFixtureSchema(ctx, opened.DB, statement), qt.IsNil)
	}
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{root, nested} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+table+"`"), qt.IsNil)
		}
	})
	client := startProviderFor(c)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })

	snapshot, err := client.Inspect(ctx, &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{"/", directory}})

	c.Assert(err, qt.IsNil)
	atRoot, inDirectory := "|"+directory+".b", directory+"|b"
	c.Assert(tablesRead(c, snapshot, atRoot, inDirectory), qt.DeepEquals, map[string]readTable{
		atRoot:      {Normalized: atRoot, Columns: []string{"id", "root_note"}, Indexes: []string{"note_ix ON root_note"}},
		inDirectory: {Normalized: inDirectory, Columns: []string{"id", "nested_note"}, Indexes: []string{"note_ix ON nested_note"}},
	})
}
