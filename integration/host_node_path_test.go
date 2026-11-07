//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// nodeLocks declares the coordination node locks in the directory app.
const nodeLocks = "coordination_nodes:\n  locks:\n    schema: app\n"

// A coordination node whose path a declared table, view or topic takes is
// refused before anything is written, and the refusal says why: YDB keeps
// one object at a path and answers `unexpected path type` for the later one.
// The host's embedded engine refused each (`coordination create conflicts
// with create at scheme path app.locks`, and for the topic `conflicting plan
// effects`, measured with schema render). Without the view check the render
// wrote both statements, and without the reasons the table's refusal said
// only `provider decode source failed`. Ported from the host's
// TestValidateSchema_YDBRefusesACoordinationNodeOnAnotherObjectsPath and
// TestValidateSchema_YDBRefusesACoordinationNodeOnATopicsPath.
func TestHostRendersANodeAtATakenPath_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, text, want string
	}{
		{name: "a table", text: nodeLocks + "tables:\n  locks:\n    schema: app\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n",
			want: `coordination node "locks": its path app/locks is the path of a table the document declares, and YDB keeps one object at a path`},
		{name: "a view", text: nodeLocks + "views:\n  v:\n    name: app.locks\n    body: \"SELECT 1 AS a\"\n",
			want: "authored native object conflicts with a declared view path"},
		{name: "a view whose name quotes its parts", text: nodeLocks + "views:\n  v:\n    name: '\"app\".\"locks\"'\n    body: \"SELECT 1 AS a\"\n",
			want: "authored native object conflicts with a declared view path"},
		{name: "a topic", text: nodeLocks + "topics:\n  locks:\n    schema: app\n", want: "authored native objects share or contain one scheme path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			file := compatFile(c, c.TempDir(), "schema.yaml", test.text)

			out, err := hostCommand(c, "schema", "render", "--schema-file", file, "--dialect", "ydb")

			c.Assert(err, qt.IsNotNil, qt.Commentf("%s", out))
			c.Assert(out, qt.Contains, test.want)
			c.Assert(out, qt.Not(qt.Contains), "CREATE COORDINATION NODE")
		})
	}
}

// The controls: an object shares the node's path only where the directory
// and the name are both the same, and a dotted name in quotes is one path
// segment. Ported from the host's
// TestValidateSchema_YDBTakesACoordinationNodeBesideOtherObjects.
func TestHostRendersANodeBesideOtherObjects_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, text, want string
	}{
		{name: "a table in another directory", text: nodeLocks + "tables:\n  locks:\n    schema: jobs\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n",
			want: "CREATE TABLE `jobs/locks`"},
		{name: "a table under another name", text: nodeLocks + "tables:\n  locks2:\n    schema: app\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n",
			want: "CREATE TABLE `app/locks2`"},
		{name: "a view whose dotted name is one segment", text: nodeLocks + "views:\n  v:\n    name: '\"app.locks\"'\n    body: \"SELECT 1 AS a\"\n",
			want: "CREATE VIEW `app.locks`"},
		{name: "a topic under another name", text: nodeLocks + "topics:\n  events:\n    schema: app\n", want: "CREATE TOPIC `app/events`"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			file := compatFile(c, c.TempDir(), "schema.yaml", test.text)

			out, err := hostCommand(c, "schema", "render", "--schema-file", file, "--dialect", "ydb")

			c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			c.Assert(out, qt.Contains, test.want)
			c.Assert(out, qt.Contains, "CREATE COORDINATION NODE `app/locks`;")
		})
	}
}

// A plan against a database refuses the node at the path of a declared view
// or table too, with the reason, and writes nothing: the comparison refuses
// the view, the decoding the table. The host's embedded engine refused both
// (`coordination create conflicts with create at scheme path`, measured with
// schema apply on 26.2.1.14); without the reasons the refusals said only
// `provider compare models failed` and `provider decode source failed`.
func TestHostRefusesANodeAtADeclaredRelationPath_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	node := "coordination_nodes:\n  locks:\n    schema: " + directory + "\n"

	view, viewErr := hostApplySource(c, directory, "yaml", node+"views:\n  v:\n    name: "+directory+".locks\n    body: \"SELECT 1 AS a\"\n")
	table, tableErr := hostApplySource(c, directory, "yaml", node+"tables:\n  locks:\n    schema: "+directory+
		"\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n")

	c.Assert(viewErr, qt.IsNotNil, qt.Commentf("%s", view))
	c.Assert(view, qt.Contains, "coordination node "+directory+"/locks takes the path of a table or view the schema declares, and YDB keeps one object at a path")
	c.Assert(tableErr, qt.IsNotNil, qt.Commentf("%s", table))
	c.Assert(table, qt.Contains, `coordination node "locks": its path `+directory+`/locks is the path of a table the document declares, and YDB keeps one object at a path`)
	c.Assert(listedNames(c, opened, path.Join(database, directory)), qt.HasLen, 0)
}
