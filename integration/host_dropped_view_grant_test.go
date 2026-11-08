//go:build integration

package integration_test

import (
	"fmt"
	"os/exec"
	"path"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// droppedViewTables declares, in directory app, a table src and a group.
func droppedViewTables(names accessNames) string {
	return "tables:\n  src:\n    schema: app\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" +
		"roles:\n  " + names.group + ":\n    group: true\n"
}

// A source that stops declaring a view and still grants on it is refused
// before anything runs, with --schemas and without it, in the words the host's
// planner gives every engine. The view keeps its query and holds no access
// entry. The built-in YDB engine at host 74de0b65b refused the run without
// --schemas in the same words, and with --schemas planned the DROP VIEW alone
// and left the declared grant out without a word (G132, stokaro/ptah#5371),
// measured on 25.1.4.7 and 26.2.1.14. The runs are in a dev realm, so a run
// without --schemas reads the realm alone; the provider writes a view there
// under the realm's path prefix. Ported from the host's
// TestGenerateMigrationAST_ViewPermissions_FailurePath.
func TestHostRefusesAGrantOnAViewThePlanDrops_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	names := newAccessNames()
	// A group belongs to the database, outside every realm, so the test
	// creates it there; a realm's read takes its database's groups as held.
	createSchemaObject(c, realm.opened, "CREATE GROUP `"+names.group+"`", "DROP GROUP IF EXISTS `"+names.group+"`")
	files := writeFiles(c, c.TempDir(), map[string]string{
		"created.yaml": droppedViewTables(names) + "views:\n  active:\n    name: app.active\n    body: \"SELECT 1 AS id\"\n",
		"refused.yaml": droppedViewTables(names) + "views: {}\n" +
			"grants:\n  view:\n    role: " + names.group + "\n    privileges: [SELECT ROW]\n    on_table: app.active\n",
	})
	created, err := hostCommand(c, "schema", "apply", "--db-url", realm.address, "--schema-file", filepath.Join(files, "created.yaml"), "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	const refusal = "granting YDB.GRANULAR.SELECT_ROW on view app.active to %s: the plan drops the view; declare it, or leave the grant out"

	for _, test := range []struct {
		name  string
		flags []string
	}{
		{name: "with --schemas", flags: []string{"--schemas", "app"}},
		{name: "without --schemas", flags: make([]string, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			arguments := append([]string{"schema", "apply", "--db-url", realm.address, "--schema-file", filepath.Join(files, "refused.yaml"), "--auto-approve"}, test.flags...)

			refused, err := hostCommand(c, arguments...)

			var exited *exec.ExitError
			c.Assert(err, qt.ErrorAs, &exited, qt.Commentf("%s", refused))
			c.Assert(exited.ExitCode(), qt.Equals, 2)
			c.Assert(refused, qt.Contains, "error: generate schema apply operations: "+fmt.Sprintf(refusal, names.group))
			c.Assert(realm.entries(c, "app"), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"active": Ydb_Scheme.Entry_VIEW, "src": Ydb_Scheme.Entry_TABLE})
			query, entries := heldView(c, realm.opened, path.Join(realm.root, "app", "active"))
			c.Assert(query, qt.Equals, "PRAGMA TablePathPrefix(\""+realm.root+"\");\nSELECT 1 AS id")
			c.Assert(entries, qt.HasLen, 0)
		})
	}
}
