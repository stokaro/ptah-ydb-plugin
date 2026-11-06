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
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Operation_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Operations"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// statementsOn are the statements of output that create or alter an object
// below directory, each by the words that open it and the object it names, in
// output order: `CREATE TABLE a`, `ALTER TABLE a ADD CHANGEFEED`. A statement
// starts on the first line of its text that opens with CREATE or ALTER, so a
// note in front of it is no part of it.
func statementsOn(output, directory string) []string {
	var named []string
	for text := range strings.SplitSeq(output, ";") {
		lines := strings.Split(strings.TrimSpace(text), "\n")
		start := slices.IndexFunc(lines, func(line string) bool {
			return strings.HasPrefix(line, "CREATE ") || strings.HasPrefix(line, "ALTER ")
		})
		if start < 0 {
			continue
		}
		statement := strings.Join(lines[start:], "\n")
		marker := "`" + directory + "/"
		at := strings.Index(statement, marker)
		if at < 0 {
			continue
		}
		object, _, _ := strings.Cut(statement[at+len(marker):], "`")
		switch {
		case strings.HasPrefix(statement, "ALTER TABLE "):
			verb, _, _ := strings.Cut(strings.TrimSpace(statement[at+len(marker)+len(object)+1:]), " `")
			named = append(named, "ALTER TABLE "+object+" "+verb)
		default:
			verb, _, _ := strings.Cut(statement, " `")
			named = append(named, verb+" "+object)
		}
	}
	return named
}

// `schema inspect --format sql` places a read's statements as the built-in
// YDB engine at host 74de0b65b placed them, on 25.1.4.7 and 26.2.1.14: the
// topic ahead of the tables and each changefeed right after its table. The
// provider wrote the topic and the changefeeds after every table.
func TestHostInspectsStatementsInTheBuiltInOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	absolute := func(name string) string { return path.Join(database, directory, name) }
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, statement := range []string{"DROP TABLE `" + absolute("a") + "`", "DROP TABLE `" + absolute("c") + "`", "DROP TOPIC `" + absolute("t") + "`"} {
			c.Check(execFixtureSchema(ctx, opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
		}
	})
	for _, statement := range []string{
		"CREATE TABLE `" + absolute("a") + "` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))",
		"ALTER TABLE `" + absolute("a") + "` ADD CHANGEFEED feed_a WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
		"CREATE TABLE `" + absolute("c") + "` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))",
		"ALTER TABLE `" + absolute("c") + "` ADD CHANGEFEED feed_c WITH (MODE = 'KEYS_ONLY', FORMAT = 'JSON')",
		"CREATE TOPIC `" + absolute("t") + "`",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(statementsOn(output, directory), qt.DeepEquals, []string{
		"CREATE TOPIC t",
		"CREATE TABLE a", "ALTER TABLE a ADD CHANGEFEED",
		"CREATE TABLE c", "ALTER TABLE c ADD CHANGEFEED",
	}, qt.Commentf("%s", output))
}

// `schema apply` adds a new table's changefeed right after the table, as the
// built-in YDB engine at host 74de0b65b planned it on 25.1.4.7 and 26.2.1.14,
// and the server holds each table's changefeed. The provider added every
// changefeed after the last table.
func TestHostAppliesANewTablesChangefeedRightAfterIt_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, name := range []string{"a", "c"} {
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE `"+path.Join(database, directory, name)+"`"), qt.IsNil)
		}
	})
	table := func(name, mode string) string {
		return "  " + name + ":\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true}\n" +
			"    changefeeds:\n      feed_" + name + ":\n        mode: " + mode + "\n        format: JSON\n"
	}

	output, err := hostApplySource(c, directory, "yaml", "tables:\n"+table("a", "UPDATES")+table("c", "KEYS_ONLY"))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(pluginUnits(output), qt.DeepEquals, []string{
		"row-table-create-operation " + directory + ".a", "changefeed-operation " + directory + ".a",
		"row-table-create-operation " + directory + ".c", "changefeed-operation " + directory + ".c",
	}, qt.Commentf("%s", output))
	held := make(map[string]options.ChangefeedMode)
	for _, name := range []string{"a", "c"} {
		feeds, found := describedChangefeeds(c, opened, path.Join(database, directory, name))
		c.Assert(found, qt.IsTrue)
		held[name] = feeds["feed_"+name].Mode
	}
	c.Assert(held, qt.DeepEquals, map[string]options.ChangefeedMode{"a": options.ChangefeedModeUpdates, "c": options.ChangefeedModeKeysOnly})
}

// indexBuildsOn are the indexes the operation service lists a build of on the
// table at absolute, in its order. YDB lists one for an index added to a table
// that exists, `ALTER TABLE ... ADD INDEX`, and none for an index CREATE TABLE
// builds with the table.
func indexBuildsOn(c *qt.C, opened *connection.Connection, absolute string) []string {
	c.Helper()
	operations := Ydb_Operation_V1.NewOperationServiceClient(ydb.GRPCConn(opened.Driver))
	var built []string
	asked := map[string]bool{"": true}
	request := &Ydb_Operations.ListOperationsRequest{Kind: "buildindex"}
	for {
		response, err := operations.ListOperations(c.Context(), request)
		c.Assert(err, qt.IsNil)
		c.Assert(response.GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS)
		for _, operation := range response.GetOperations() {
			var metadata Ydb_Table.IndexBuildMetadata
			c.Assert(operation.GetMetadata().UnmarshalTo(&metadata), qt.IsNil)
			built = append(built, metadata.GetDescription().GetPath()+" "+metadata.GetDescription().GetIndex().GetName())
		}
		next := response.GetNextPageToken()
		if next == "0" || asked[next] {
			break
		}
		asked[next] = true
		request = &Ydb_Operations.ListOperationsRequest{Kind: "buildindex", PageToken: next}
	}
	var on []string
	for _, build := range built {
		table, index, _ := strings.Cut(build, " ")
		if table == absolute {
			on = append(on, index)
		}
	}
	return on
}

// `schema apply` creates a new table's vector index with the table, in one
// CREATE TABLE that writes every index of the table in name order, as the
// built-in YDB engine at host 74de0b65b did on 25.1.4.7 and 26.2.1.14: the
// plan runs one operation, YDB lists no index build on the table, and the
// table holds the index with its settings. `schema inspect` writes the read
// table the same way. The provider created the table and then added the
// vector index with `ALTER TABLE ... ADD INDEX`, a build YDB lists. A host
// declares a vector index only where it lists source.index-kinds.
func TestHostCreatesANewTablesVectorIndexWithIt_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := docsFixture(c)
	source := "tables:\n  docs:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true}\n      gen: {type: Uint64}\n      emb: {type: String}\n" +
		"    indexes:\n      idx_gen:\n        fields: [gen]\n      idx_emb:\n        fields: [emb]\n        type: vector_kmeans_tree\n        distance: COSINE\n" +
		"        vector_type: float\n        vector_dimension: 3\n        levels: 1\n        clusters: 4\n"
	indexes := "    INDEX `idx_emb` GLOBAL USING vector_kmeans_tree ON (`emb`) WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=4),\n" +
		"    INDEX `idx_gen` GLOBAL SYNC ON (`gen`)"

	output, err := hostApplySource(c, directory, "yaml", source)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(pluginUnits(output), qt.DeepEquals, []string{"row-table-create-operation " + directory + ".docs"}, qt.Commentf("%s", output))
	c.Assert(indexBuildsOn(c, opened, absolute), qt.HasLen, 0)
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(session.Close(), qt.IsNil) }()
	described, err := session.Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	var held []string
	for _, index := range described.GetIndexes() {
		held = append(held, index.GetName()+" "+index.GetStatus().String())
	}
	c.Assert(held, qt.DeepEquals, []string{"idx_emb STATUS_READY", "idx_gen STATUS_READY"})
	vector := described.GetIndexes()[0].GetGlobalVectorKmeansTreeIndex().GetVectorSettings()
	c.Assert([]uint32{vector.GetSettings().GetVectorDimension(), vector.GetLevels(), vector.GetClusters()}, qt.DeepEquals, []uint32{3, 1, 4})
	again, err := hostApplySource(c, directory, "yaml", source)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", again))
	c.Assert(pluginUnits(again), qt.HasLen, 0, qt.Commentf("%s", again))
	inspected, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory, "--format", "sql")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", inspected))
	c.Assert(inspected, qt.Contains, "    PRIMARY KEY (`id`),\n"+indexes+",\n", qt.Commentf("%s", inspected))
	c.Assert(statementsOn(inspected, directory), qt.DeepEquals, []string{"CREATE TABLE docs"}, qt.Commentf("%s", inspected))
}
