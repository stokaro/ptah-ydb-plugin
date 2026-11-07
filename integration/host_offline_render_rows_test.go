//go:build integration

package integration_test

import (
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// uniqueTable is a Go model of the table t whose declaration carries the
// directives above its struct and the fields at its end.
func uniqueTable(above, fields string) string {
	return "package models\n\n" + above + "\n//ptah:schema:table name=\"t\"\ntype T struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"a\" type=\"Utf8\"\n\tA *string\n" + fields + "}\n"
}

// A UNIQUE declaration YDB has no clause for is refused when the schema is
// rendered, in the words of the host's embedded engine where it wrote its
// own: a condition, NULLS NOT DISTINCT, two indexes under one name, and a
// column part that is no column of the table. Without the provider's own
// words the first two named the protocol field that carries them. Ported from
// the cases of the host's TestRender_UniqueConstraint_FailurePath a source
// can write; deferrable and NOT ENFORCED have no source spelling, a bare
// UNIQUE column renders as an index (TestHostRendersUniqueDeclarations_HappyPath),
// and an index added to a table that exists is
// TestHostRefusesAUniqueIndexOnATableThatExists_FailurePath.
func TestHostRendersUniqueDeclarationsYDBHasNoClauseFor_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, source, want string
	}{
		{name: "a partial unique index", source: uniqueTable("", "\t//ptah:schema:index name=\"uq\" fields=\"a\" unique=\"true\" where=\"a <> ''\"\n\t_ int\n"),
			want: `index "uq": YDB has no partial index`},
		{name: "NULLS NOT DISTINCT", source: uniqueTable(`//ptah:schema:constraint type="UNIQUE" name="uq" columns="a" nulls_distinct="false"`, ""),
			want: `index "uq" treats NULLs as equal; YDB has no unique_nulls_distinct_clause`},
		{name: "two indexes under one name", source: uniqueTable(`//ptah:schema:constraint type="UNIQUE" name="uq" columns="a"`, "\t//ptah:schema:index name=\"uq\" fields=\"a\"\n\t_ int\n"),
			want: `table "t": two of its indexes are named "uq", and YDB names an index once per table`},
		{name: "a descending column", source: uniqueTable(`//ptah:schema:constraint type="UNIQUE" name="uq" columns="a DESC"`, ""),
			want: `index "uq": it names column "a DESC", which the table does not declare`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"t.go": test.source})

			out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb", "--server-version", "26.2.1.14")

			c.Assert(err, qt.IsNotNil, qt.Commentf("%s", out))
			c.Assert(out, qt.Contains, test.want)
			c.Assert(out, qt.Not(qt.Contains), "CREATE TABLE")
		})
	}
}

// itemsTable is the YAML table items with its key and the further columns
// and indexes given.
func itemsTable(more string) string {
	return "tables:\n  items:\n    columns:\n      id: {type: Int64, primary: true, not_null: true}\n" + more
}

// A diff of two files plans each change of an existing table as its own
// statement, as the host's embedded engine wrote it: an index added to the
// table, columns added and dropped beside an index dropped and one added, a
// NOT NULL column added with a default on a line that fills the rows the
// table holds, a column made nullable, and a default set and one dropped in
// place. Each statement is the built-in's at host 74de0b65b on the same files
// and line. The --from file carries no native facet for a column whose
// declaration states no native setting; without reading that known absence
// as such a column, the last two were refused (`requires the complete native
// column observation`, gap G226). Ported from the host's
// TestRender_Changes_HappyPath, but for a table renamed, which a diff plans
// as a drop and a creation on both engines.
func TestHostSchemaDiffPlansEachChangeOfATable_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, from, to, version string
		want                    []string
	}{
		{name: "an index added to a table that exists", version: "25.1.4.7",
			from: itemsTable("      kind: {type: Utf8}\n      seq: {type: Int64}\n"),
			to:   itemsTable("      kind: {type: Utf8}\n      seq: {type: Int64}\n    indexes:\n      events_kind: {fields: [kind, seq]}\n"),
			want: []string{"ALTER TABLE `items` ADD INDEX `events_kind` GLOBAL SYNC ON (`kind`, `seq`);"}},
		{name: "one statement per operation", version: "25.1.4.7",
			from: itemsTable("      legacy: {type: Utf8}\n    indexes:\n      items_a: {fields: [legacy]}\n"),
			to:   itemsTable("      note: {type: Utf8}\n    indexes:\n      items_note: {fields: [note]}\n"),
			want: []string{"ALTER TABLE `items` DROP INDEX `items_a`;", "ALTER TABLE `items` ADD COLUMN `note` Utf8;",
				"ALTER TABLE `items` DROP COLUMN `legacy`;", "ALTER TABLE `items` ADD INDEX `items_note` GLOBAL SYNC ON (`note`);"}},
		{name: "a NOT NULL column added with a default", version: "26.1.1.6",
			from: itemsTable(""), to: itemsTable("      qty: {type: Int64, not_null: true, default: '7'}\n"),
			want: []string{"ALTER TABLE `items` ADD COLUMN `qty` Int64 NOT NULL DEFAULT 7l;"}},
		{name: "a column made nullable", version: "25.1.4.7",
			from: itemsTable("      label: {type: Utf8, not_null: true}\n"), to: itemsTable("      label: {type: Utf8}\n"),
			want: []string{"ALTER TABLE `items` ALTER COLUMN `label` DROP NOT NULL;"}},
		{name: "a default set and one dropped in place", version: "26.2.1.14",
			from: itemsTable("      qty: {type: Int32}\n      label: {type: Utf8, default: \"'x'\"}\n"),
			to:   itemsTable("      qty: {type: Int32, default: '2'}\n      label: {type: Utf8}\n"),
			want: []string{"ALTER TABLE `items` ALTER COLUMN `label` DROP DEFAULT;", "ALTER TABLE `items` ALTER COLUMN `qty` SET DEFAULT 2;"}},
		{name: "a default changed beside a column made nullable", version: "26.2.1.14",
			from: itemsTable("      label: {type: Utf8, default: \"'x'\"}\n      n: {type: Int64, not_null: true, default: '5'}\n"),
			to:   itemsTable("      label: {type: Utf8, default: \"'it''s'\"}\n      n: {type: Int64, default: '5'}\n"),
			want: []string{"ALTER TABLE `items` ALTER COLUMN `label` SET DEFAULT 'it\\'s'u;", "ALTER TABLE `items` ALTER COLUMN `n` DROP NOT NULL;"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			from := compatFile(c, c.TempDir(), "from.yaml", test.from)
			to := compatFile(c, c.TempDir(), "to.yaml", test.to)

			out, err := hostCommand(c, "schema", "diff", "--from", from, "--to", to, "--dev-url", devOnlyURL, "--server-version", test.version)

			c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			c.Assert(previewStatements(out), qt.ContentEquals, test.want)
		})
	}
}

// A file compared with itself plans nothing, the defaults of its columns
// included: each side reads a default as the same native literal. The host's
// embedded engine planned an ALTER COLUMN ... SET DEFAULT for each column with
// a default (measured with schema diff on 26.2.1.14), which is not copied.
func TestHostSchemaDiffOfAFileWithItselfPlansNothing_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	text := itemsTable("      qty: {type: Int32, default: '2'}\n      label: {type: Utf8, default: \"'x'\"}\n" +
		"      at: {type: Timestamp, default: '2024-01-02T03:04:05Z'}\n")
	from, to := compatFile(c, c.TempDir(), "from.yaml", text), compatFile(c, c.TempDir(), "to.yaml", text)

	out, err := hostCommand(c, "schema", "diff", "--from", from, "--to", to, "--dev-url", devOnlyURL, "--server-version", "26.2.1.14")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(previewStatements(out), qt.HasLen, 0, qt.Commentf("%s", out))
}
