//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The external host schedules a stop before replacing an input table with a
// topic, removes the old source, and restarts the query with the new body.
// This exercises the host's combined graph rather than provider slice order.
func TestHostStreamingQueryGraph(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	if !session.Capabilities[streaming.Capability] {
		c.Skip("the target does not support streaming queries")
	}
	quoted := func(name string) string { return "`" + path.Join(directory, name) + "`" }
	body := "INSERT INTO `" + path.Join(database, directory, "sink") + "` SELECT * FROM `" + path.Join(database, directory, "source") + "`;"
	changed := "PRAGMA TablePathPrefix = " + strconv.Quote(path.Join(database, directory)) + "; INSERT INTO sink SELECT * FROM next;"
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, opened.DB, "DROP STREAMING QUERY "+quoted("copy"))
		for _, name := range []string{"source", "sink", "next"} {
			_ = execFixtureSchema(ctx, opened.DB, "DROP TOPIC "+quoted(name))
		}
		_ = execFixtureSchema(ctx, opened.DB, "DROP TABLE "+quoted("next"))
	})
	for _, statement := range []string{
		"CREATE TOPIC " + quoted("source"), "CREATE TOPIC " + quoted("sink"),
		"CREATE TABLE " + quoted("next") + " (id Uint64 NOT NULL, PRIMARY KEY (id))",
		streaming.Create(directory, "copy", streaming.Spec{Text: body}, false),
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil)
	}
	_, err = client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	declared := "CREATE TOPIC " + quoted("next") + ";\nCREATE TOPIC " + quoted("sink") + ";\n" + streaming.Create(directory, "copy", streaming.Spec{Text: changed}, true)
	output, err := hostApplySource(c, directory, "sql", declared)
	c.Logf("host graph: %s", output)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Schema apply completed successfully")
	firstStop := strings.Index(output, "ALTER STREAMING QUERY changes")
	removedSource := strings.Index(output, "DROP TOPIC removes")
	removedTable := strings.Index(output, "Drops a table and its rows")
	createdTopic := strings.Index(output, "Creation inherits server access policy")
	restarted := strings.Index(output, "resets streaming-query checkpoint state")
	c.Assert(firstStop >= 0 && removedSource > firstStop && removedTable > firstStop && createdTopic > removedTable && restarted > createdTopic, qt.IsTrue, qt.Commentf("%s", output))
	held, err := nativeapi.ReadStreamingQuery(c.Context(), opened.DB, database, path.Join(database, directory, "copy"))
	c.Assert(err, qt.IsNil)
	c.Assert(held, qt.IsNotNil)
	c.Assert(streaming.Equal(*held, streaming.Spec{Text: changed}), qt.IsTrue)
	output, err = hostApplySource(c, directory, "sql", declared)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Schema is synced")
	stopped := fmt.Sprintf("topics:\n  next:\n    schema: %s\n  sink:\n    schema: %s\nstreaming_queries:\n  copy:\n    schema: %s\n    text: %s\n    run: false\n", directory, directory, directory, strconv.Quote(changed))
	output, err = hostApplySource(c, directory, "yaml", stopped)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(strings.Count(output, "Native operation: "+schema.StreamingQueryOperation), qt.Equals, 1)
	held, err = nativeapi.ReadStreamingQuery(c.Context(), opened.DB, database, path.Join(database, directory, "copy"))
	c.Assert(err, qt.IsNil)
	c.Assert(held, qt.IsNotNil)
	c.Assert(streaming.Running(*held), qt.IsFalse)
	goSource := fmt.Sprintf("package entities\n\n//ptah:schema:topic name=\"next\" schema=%s\ntype Next struct{}\n\n//ptah:schema:topic name=\"sink\" schema=%s\ntype Sink struct{}\n\n//ptah:schema:streamingquery name=\"copy\" schema=%s text=%s run=\"false\"\ntype Copy struct{}\n", strconv.Quote(directory), strconv.Quote(directory), strconv.Quote(directory), strconv.Quote(changed))
	output, err = hostApplySource(c, directory, "go", goSource)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Schema is synced")
}

func TestHostRefusesStreamingWithoutCapability(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	if session.Capabilities[streaming.Capability] {
		c.Skip("the target supports streaming queries")
	}
	output, err := hostApplySource(c, directory, "sql", streaming.Create(directory, "refused", streaming.Spec{Text: "SELECT 1;"}, false))
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "provider compare models failed (InvalidArgument)")
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	c.Assert(listing.Children, qt.HasLen, 0)
}
