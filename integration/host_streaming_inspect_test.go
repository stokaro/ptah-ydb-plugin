//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// `schema inspect --format sql` of a streaming query whose text names its
// topic and its table by absolute path writes the query, as the built-in YDB
// engine at host 74de0b65b did on 26.2.1.14: the text as YDB holds it, after
// the topic and the table it names. 25.1.4.7 has no streaming queries. The
// provider refused the read (`streaming query path is outside the captured
// database or dev realm`), because it resolved the paths against no database.
func TestHostInspectsAStreamingQueryNamingAbsolutePaths_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	_, session := accessSession(c)
	skipWithout(c, session, streaming.Capability)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
	})
	absolute := func(name string) string { return path.Join(database, directory, name) }
	text := "UPSERT INTO `" + absolute("d/sink") + "` SELECT 1ul AS id, CAST(Data AS Utf8) AS v FROM `" + absolute("c/input") + "`;"
	for _, statement := range []string{
		"CREATE TOPIC `" + absolute("c/input") + "`",
		"CREATE TABLE `" + absolute("d/sink") + "` (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id))",
		streaming.Create(directory, "q", streaming.Spec{Text: text}),
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "schema", "inspect", "--schemas", directory+","+directory+"/c,"+directory+"/d", "--format", "sql")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	query := strings.Index(output, "CREATE STREAMING QUERY `"+directory+"/q`")
	c.Assert(query, qt.Not(qt.Equals), -1, qt.Commentf("%s", output))
	c.Assert(output[query:], qt.Contains, "\n"+text+"\n")
	for _, named := range []string{"CREATE TOPIC `" + directory + "/c/input`", "CREATE TABLE `" + directory + "/d/sink`"} {
		at := strings.Index(output, named)
		c.Assert(at, qt.Not(qt.Equals), -1, qt.Commentf("%s\n%s", named, output))
		c.Assert(at < query, qt.IsTrue, qt.Commentf("%s\n%s", named, output))
	}
}
