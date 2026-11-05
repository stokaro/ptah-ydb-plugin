//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// streamingFixture is a locked fixture directory on a line with streaming
// queries. The cleanup drops the query before the topics it reads.
func streamingFixture(c *qt.C) (opened *connection.Connection, directory, database string, held func() *streaming.Spec) {
	c.Helper()
	opened, directory, database = lockFixture(c)
	_, session := accessSession(c)
	skipWithout(c, session, streaming.Capability)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, path.Join(database, directory))
	})
	held = func() *streaming.Spec {
		c.Helper()
		spec, err := nativeapi.ReadStreamingQuery(c.Context(), opened.DB, database, path.Join(database, directory, "copy"))
		c.Assert(err, qt.IsNil)
		return spec
	}
	return opened, directory, database, held
}

// Ported from the host's TestYDBDesiredYQL_StreamingQueries: a YQL schema
// creates a stopped query between two topics, and the next run plans nothing.
// A changed body in a plain CREATE is refused, naming allow_state_reset,
// since it would lose the query's checkpoint, and changes nothing. RUN=TRUE
// starts it, CREATE OR REPLACE changes its body and stops it, a schema of the
// topics alone drops the query, and an empty one drops the topics. Each of
// those runs changes the database and leaves nothing to plan.
func TestHostRunsAStreamingQueryFromYQL_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database, held := streamingFixture(c)
	replacer := strings.NewReplacer("{dir}", directory)
	topics := replacer.Replace("CREATE TOPIC `{dir}/source`; CREATE TOPIC `{dir}/sink`;\n")
	body := replacer.Replace(" AS DO BEGIN INSERT INTO `{dir}/sink` SELECT * FROM `{dir}/source`")
	applyRuns(c, directory, "sql", topics+replacer.Replace("CREATE STREAMING QUERY `{dir}/copy` WITH (RUN=FALSE)")+body+"; END DO;\n")
	c.Assert(streaming.Running(*held()), qt.IsFalse)

	output, err := hostApplySource(c, directory, "sql", topics+replacer.Replace("CREATE STREAMING QUERY `{dir}/copy` WITH (RUN=FALSE)")+body+" WHERE TRUE; END DO;\n")
	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `streaming query "`+directory+`/copy": changing text resets aggregation state; declare allow_state_reset=true to permit it`)
	c.Assert(held().Text, qt.Not(qt.Contains), "WHERE TRUE")

	for _, step := range []struct{ name, source string }{
		{"started", topics + replacer.Replace("CREATE STREAMING QUERY `{dir}/copy` WITH (RUN=TRUE)") + body + "; END DO;\n"},
		{"replaced", topics + replacer.Replace("CREATE OR REPLACE STREAMING QUERY `{dir}/copy` WITH (RUN=FALSE)") + body + " WHERE TRUE; END DO;\n"},
	} {
		applyRuns(c, directory, "sql", step.source)
	}
	c.Assert(held().Text, qt.Contains, "WHERE TRUE")
	c.Assert(streaming.Running(*held()), qt.IsFalse)
	applyRuns(c, directory, "sql", topics)
	c.Assert(held(), qt.IsNil)
	applyRuns(c, directory, "sql", "\n")
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.HasLen, 0)
}

// Ported from the host's TestYDBStreamingQueries_RoundTripAndRollback, without
// the rollback, which plans a reverse script that no host verb runs through
// a provider: two topics and a stopped query are created, the query reads
// back stopped in the default pool, it is started and stopped again, a
// changed text is refused without allow_state_reset and applied with it, and
// a declaration without the query drops it before nothing else needs it.
func TestHostChangesAStreamingQueryLifecycle_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, _, held := streamingFixture(c)
	text := "INSERT INTO `" + directory + "/output` SELECT * FROM `" + directory + "/input`;"
	declared := func(query string) string {
		return strings.ReplaceAll("topics:\n  input:\n    schema: {dir}\n  output:\n    schema: {dir}\n", "{dir}", directory) + query
	}
	query := func(text, run, reset string) string {
		return "streaming_queries:\n  copy:\n    schema: " + directory + "\n    text: \"" + strings.ReplaceAll(text, `"`, `\"`) + "\"\n    run: " + run + "\n" + reset
	}
	applyUntilSynced(c, directory, "yaml", declared(query(text, "false", "")))
	c.Assert([]any{streaming.Running(*held()), streaming.Pool(*held())}, qt.DeepEquals, []any{false, "default"})

	applyRuns(c, directory, "yaml", declared(query(text, "true", "")))
	c.Assert(streaming.Running(*held()), qt.IsTrue)
	applyRuns(c, directory, "yaml", declared(query(text, "false", "")))
	c.Assert(streaming.Running(*held()), qt.IsFalse)

	changed := strings.TrimSuffix(text, ";") + " WHERE TRUE; /* changed */"
	output, err := hostApplySource(c, directory, "yaml", declared(query(changed, "false", "")))
	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `streaming query "`+directory+`/copy": changing text resets aggregation state; declare allow_state_reset=true to permit it`)
	applyRuns(c, directory, "yaml", declared(query(changed, "false", "    allow_state_reset: true\n")))
	c.Assert(held().Text, qt.Contains, "/* changed */")

	c.Assert(hostPlanned(c, directory, "yaml", "topics: {}\nstreaming_queries: {}\n")[0], qt.Matches, `streaming-query-operation .*copy`)
	applyRuns(c, directory, "yaml", "topics: {}\nstreaming_queries: {}\n")
	c.Assert(held(), qt.IsNil)
}
