//go:build integration

package integration_test

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// planVerdict is how a saved plan rates one of its statements.
type planVerdict struct {
	Severity  string `json:"severity"`
	Reason    string `json:"reason"`
	Statement string `json:"sql"`
}

// Ported from the YDB row of the host's
// TestPlanFileWithStatementsFromSQLAsksThePlansOwner (internal/atlasschema).
// `ptah-compat schema plan --edit` rebuilds the plan from the text the editor
// leaves and asks the plan's owner to rate each statement again, so a DROP
// TOPIC written in the editor is destructive and marks the plan destructive,
// with the reason the built-in engine at 74de0b65b gave on 25.1.4.7 and
// 26.2.1.14. The host has no rule of its own for DROP TOPIC: the rating is
// this provider's. The planned change is a view, which the host plans as a
// statement, since an edit of a plan holding a native operation is refused.
// The run reads a dev realm, so its read of the whole database sees only what
// the test created.
func TestHostCompatRatesAnEditedTopicDropDestructive_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	realm := newCLIRealm(c)
	for _, statement := range []string{
		"CREATE TOPIC `" + path.Join(realm.root, "events") + "`",
		"CREATE TABLE `" + path.Join(realm.root, "t") + "` (id Int64 NOT NULL, PRIMARY KEY (id))",
	} {
		c.Assert(execFixtureSchema(c.Context(), realm.opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	work := c.TempDir()
	desired := compatFile(c, work, "desired.sql", "CREATE TOPIC `events`;\nCREATE TABLE `t` (id Int64 NOT NULL, PRIMARY KEY (id));\n"+
		"CREATE VIEW `v` WITH (security_invoker = TRUE) AS SELECT id FROM `t`;\n")
	editor := filepath.Join(work, "editor.sh")
	// #nosec G306 -- the editor the command runs has to be executable; it is a test's own script in its temporary directory
	c.Assert(os.WriteFile(editor, []byte("#!/bin/sh\nprintf 'DROP TOPIC `events`;\\n' > \"$1\"\n"), 0o700), qt.IsNil)
	saved := filepath.Join(work, "edited.plan.json")

	stdout, stderr, exit := runCompat(c, []string{"EDITOR=" + editor, "VISUAL="}, "schema", "plan", "--from", realm.address, "--to", "file://"+desired,
		"--edit", "--output", saved)

	c.Assert(exit, qt.Equals, 0, qt.Commentf("stdout:\n%s\nstderr:\n%s", stdout, stderr))
	contents, err := os.ReadFile(saved) // #nosec G304 -- the plan file this test asked the command to write in its temporary directory
	c.Assert(err, qt.IsNil)
	var plan struct {
		Destructive bool          `json:"destructive"`
		Statements  []planVerdict `json:"statements"`
	}
	c.Assert(json.Unmarshal(contents, &plan), qt.IsNil, qt.Commentf("%s", contents))
	c.Assert(plan.Statements, qt.HasLen, 1, qt.Commentf("%s", contents))
	c.Assert(plan.Statements[0].Severity, qt.Equals, "destructive")
	c.Assert(plan.Statements[0].Reason, qt.Equals, "DROP TOPIC removes the topic, every message it holds and every consumer's position in it")
	c.Assert(strings.TrimSuffix(strings.TrimSpace(plan.Statements[0].Statement), ";"), qt.Equals, "DROP TOPIC `events`")
	c.Assert(plan.Destructive, qt.IsTrue)
}
