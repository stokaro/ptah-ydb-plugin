//go:build integration

package integration_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the rollback of the host's
// TestYDBStreamingQueries_RoundTripAndRollback: `migrations generate` writes
// a changed text of a stopped query, which allow_state_reset permits, and its
// reverse; `migrations up` applies it and leaves nothing to plan, and
// `migrations down` gives the query its text back, so the first declaration
// plans nothing. The down file says what the rollback cannot recover: the
// checkpoint the change reset.
func TestHostRollsBackAStreamingQueryText_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	_, directory, _, held := streamingFixture(c)
	text := "INSERT INTO `" + directory + "/output` SELECT * FROM `" + directory + "/input`;"
	changed := strings.TrimSuffix(text, ";") + " WHERE TRUE; /* changed */"
	declared := func(text, reset string) string {
		return strings.ReplaceAll("topics:\n  input:\n    schema: {dir}\n  output:\n    schema: {dir}\n", "{dir}", directory) +
			"streaming_queries:\n  copy:\n    schema: " + directory + "\n    text: \"" + strings.ReplaceAll(text, `"`, `\"`) + "\"\n    run: false\n" + reset
	}
	applyUntilSynced(c, directory, "yaml", declared(text, ""))

	migrations, up, down := hostGenerate(c, directory, "yaml", declared(changed, "    allow_state_reset: true\n"))
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	forward := held().Text
	settled := hostPlanned(c, directory, "yaml", declared(changed, "    allow_state_reset: true\n"))
	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")

	c.Assert(up, qt.Contains, "/* changed */")
	c.Assert(down, qt.Contains, `"restore the query body in place"`)
	c.Assert(down, qt.Contains, streaming.CheckpointLoss)
	c.Assert(upErr, qt.IsNil, qt.Commentf("up:\n%s", applied))
	c.Assert(forward, qt.Contains, "WHERE TRUE")
	c.Assert(settled, qt.HasLen, 0)
	c.Assert(downErr, qt.IsNil, qt.Commentf("down:\n%s\n%s", rolledBack, down))
	c.Assert(held().Text, qt.Not(qt.Contains), "WHERE TRUE")
	c.Assert(streaming.Running(*held()), qt.IsFalse)
	c.Assert(hostPlanned(c, directory, "yaml", declared(text, "")), qt.HasLen, 0)
}
