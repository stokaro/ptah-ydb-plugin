//go:build integration

package integration_test

import (
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestParseSource_NotDescribed_OwnerKinds_HappyPath
// (core/goschema), which read the built-in engine's coverage kinds
// changefeed, column_table, table_option and ttl from a Go source's
// ptah:schema:notdescribed. The host accepted them in a YQL header too, and
// accepting them kept nothing: measured at 74de0b65b on 26.2.1.14, a YQL
// source that left a table's changefeed, its TTL or a column table not
// described, by name or as a whole kind, planned the same DROP CHANGEFEED,
// RESET (TTL) and DROP TABLE as one that said nothing, and so did a Go
// source. This provider names none of these kinds, so the host refuses each
// one by name before it reads anything, in a Go source and in a YQL header:
// an author who meant to keep the object is told so rather than ignored. We
// are stricter here, deliberately.
func TestHostRefusesTheBuiltInEnginesLimitKinds_FailurePath(t *testing.T) {
	for _, kind := range []string{"changefeed", "column_table", "table_option", "ttl"} {
		t.Run(kind, func(t *testing.T) {
			c := testcheck.New(t)
			root := c.TempDir()
			entities := writeFiles(c, filepath.Join(root, "entities"), map[string]string{"items.go": "package entities\n\n" +
				"//ptah:schema:table name=\"items\" schema=\"app\"\ntype Item struct {\n\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary\n\tID int64\n}\n\n" +
				"//ptah:schema:notdescribed kind=\"" + kind + "\" name=\"app/items\"\ntype _ struct{}\n"})
			file := filepath.Join(writeFiles(c, filepath.Join(root, "yql"), map[string]string{"items.sql": "-- ptah:not-described " + kind + " \"app/items\"\n" +
				"CREATE TABLE `app/items` (id Int64 NOT NULL, PRIMARY KEY (id));\n"}), "items.sql")

			fromGo, goErr := hostCommand(c, "schema", "render", "--root-dir", entities, "--dialect", "ydb", "--server-version", "26.2.1.14")
			fromYQL, yqlErr := hostCommand(c, "schema", "render", "--schema-file", file, "--dialect", "ydb", "--server-version", "26.2.1.14")

			c.Assert(goErr, qt.IsNotNil, qt.Commentf("%s", fromGo))
			c.Assert(fromGo, qt.Contains, "unknown coverage kind \""+kind+"\"")
			c.Assert(fromGo, qt.Not(qt.Contains), "CREATE TABLE")
			c.Assert(yqlErr, qt.IsNotNil, qt.Commentf("%s", fromYQL))
			c.Assert(fromYQL, qt.Contains, "unknown coverage kind \""+kind+"\"")
			c.Assert(fromYQL, qt.Not(qt.Contains), "CREATE TABLE")
		})
	}
}
