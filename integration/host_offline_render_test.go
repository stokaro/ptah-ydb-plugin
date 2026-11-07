//go:build integration

package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// offlineEntity declares one row table in Go, with a key and a column.
const offlineEntity = "package entities\n\n//ptah:schema:table name=\"items\"\ntype Item struct {\n" +
	"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\"\n\tID int64\n" +
	"\t//ptah:schema:field name=\"label\" type=\"Utf8\"\n\tLabel string\n}\n"

// The host renders a table offline, with no server version, against the
// facts this provider states for offline planning
// (engine.default-capabilities): table creation needs nontransactional DDL
// established, and without the stated facts no fact is established offline.
func TestHostRendersATableOffline_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"items.go": offlineEntity})

	out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "CREATE TABLE `items` (\n    `id` Int64 NOT NULL,\n    `label` Utf8,\n    PRIMARY KEY (`id`)\n);")
}

// compositeEntity declares a table whose key is two columns, with portable
// types the provider maps to YDB's.
const compositeEntity = "package entities\n\n//ptah:schema:table name=\"memberships\" primary_key=\"org_id,user_id\"\ntype Membership struct {\n" +
	"\t//ptah:schema:field name=\"org_id\" type=\"INTEGER\" not_null=\"true\"\n\tOrgID int32\n" +
	"\t//ptah:schema:field name=\"user_id\" type=\"INTEGER\" not_null=\"true\"\n\tUserID int32\n" +
	"\t//ptah:schema:field name=\"role\" type=\"TEXT\" not_null=\"true\"\n\tRole string\n}\n"

// The host renders a composite key offline as one table-level PRIMARY KEY in
// the declared order, and every spelling of the dialect, ydbs and YDB among
// them, renders the same bytes. Ported from the host's
// TestGenerateCommand_RendersYDB_HappyPath, TestRenderSQL_EveryYDBSpellingRendersAsYDB
// and TestGenerateSchemaDiffSQL_PlansYDB.
func TestHostRendersACompositeKeyInEverySpelling_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"memberships.go": compositeEntity})

	ydb, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", ydb))
	c.Assert(ydb, qt.Contains, "CREATE TABLE `memberships` (\n    `org_id` Int32 NOT NULL,\n    `user_id` Int32 NOT NULL,\n    `role` Utf8 NOT NULL,\n"+
		"    PRIMARY KEY (`org_id`, `user_id`)\n);")
	for _, spelling := range []string{"ydbs", "YDB"} {
		t.Run(spelling, func(t *testing.T) {
			c := testcheck.New(t)
			rendered, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", spelling)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", rendered))
			c.Assert(rendered, qt.Equals, ydb)
		})
	}
}

// A declaration the provider cannot render fails the offline render with
// exit 2, names what it cannot hold, and writes no table: an expression
// default, a foreign key, and an index method YDB has no counterpart for,
// which the built-in engine refused for its included columns. The words are
// the provider's for the outcome the built-in engine had. Ported from the
// host's TestGenerateCommand_RendersYDB_FailurePath and
// TestIndexIncludeUnsupportedAccessMethodsFailClosed.
func TestHostRendersYDBOffline_FailurePath(t *testing.T) {
	const key = "\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\"\n\tID int64\n"
	accounts := func(method string) string {
		return "package entities\n\n//ptah:schema:table name=\"accounts\"\ntype Account struct {\n" + key +
			"\t//ptah:schema:field name=\"email\" type=\"Utf8\"\n\tEmail string\n\t//ptah:schema:field name=\"display_name\" type=\"Utf8\"\n\tName string\n" +
			"\t//ptah:schema:index name=\"idx_email\" fields=\"email\" type=\"" + method + "\" include=\"display_name\"\n\t_ int\n}\n"
	}
	for _, test := range []struct {
		name, source, want string
	}{
		{"an expression default", "package entities\n\n//ptah:schema:table name=\"products\"\ntype Product struct {\n" + key +
			"\t//ptah:schema:field name=\"created_at\" type=\"TIMESTAMP\" not_null=\"true\" default_expr=\"CURRENT_TIMESTAMP\"\n\tCreatedAt string\n}\n",
			"error: error rendering ydb schema: expression defaults require expression_defaults, unavailable on this YDB target"},
		{"a foreign key", "package entities\n\n//ptah:schema:table name=\"users\"\ntype User struct {\n" + key + "}\n\n" +
			"//ptah:schema:table name=\"posts\"\ntype Post struct {\n" + key + "\t//ptah:schema:field name=\"user_id\" type=\"Int64\" foreign=\"users(id)\"\n\tUserID int64\n}\n",
			"error: error rendering ydb schema: YDB table creation does not support ptah.provider.experimental.DeclaredColumn.foreign_reference"},
		{"a hash index with included columns", accounts("HASH"), `error: error rendering ydb schema: index "idx_email": index method "HASH" has no YDB counterpart`},
		{"a gin index with included columns", accounts("GIN"), `error: error rendering ydb schema: index "idx_email": index method "GIN" has no YDB counterpart`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"entities.go": test.source})

			out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb")

			var exited *exec.ExitError
			c.Assert(err, qt.ErrorAs, &exited, qt.Commentf("%s", out))
			c.Assert(exited.ExitCode(), qt.Equals, 2)
			c.Assert(out, qt.Contains, test.want)
			c.Assert(out, qt.Not(qt.Contains), "CREATE TABLE")
		})
	}
}

// A grant the target line cannot write fails the offline render, and the
// host prints the provider's reason, which names the statement, the object
// and the principal in the words of the host's embedded engine: on 25.1 YDB
// takes a single name only as an absolute path, and an offline render knows
// no database path. Without the reason the host prints the gRPC code alone,
// "provider render failed (FailedPrecondition)". Ported from the host's
// TestDescribe, for the words the host prints.
func TestHostRendersAGrantTheLineCannotWrite_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	file := filepath.Join(c.TempDir(), "schema.sql")
	c.Assert(os.WriteFile(file, []byte("CREATE TABLE items (id Uint64 NOT NULL, PRIMARY KEY (id));\nCREATE USER alice;\n"+
		"GRANT 'ydb.generic.read' ON items TO alice;\n"), 0o600), qt.IsNil)

	out, err := hostCommand(c, "schema", "render", "--schema-file", file, "--dialect", "ydb", "--server-version", "25.1.4.7")

	var exited *exec.ExitError
	c.Assert(err, qt.ErrorAs, &exited, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "unsupported feature: GRANT on table items to alice: YDB takes this object only by its absolute path, "+
		"which begins with the database's own path; plan it against the database, which names it by that path")
}

// A vector index setting value YDB does not take fails the offline render of
// an HCL or a YAML document, and the host prints the provider's reason, which
// names the table, the index and what the setting takes, as the host's
// embedded engine named them; the value the document wrote stays out of it.
// Without the reason the host printed `provider decode source failed
// (InvalidArgument)`. Ported from the host's TestParse_YDBVectorIndex_FailurePath
// of internal/atlashcl and internal/ydbsource.
func TestHostRendersAnInvalidVectorSetting_FailurePath(t *testing.T) {
	const want = `table "docs": index "docs_emb": invalid distance: write one of cosine, euclidean, manhattan`
	for _, test := range []struct {
		file, text string
	}{
		{file: "schema.hcl", text: "table \"docs\" {\n  column \"id\" {\n    null = false\n    type = Uint64\n  }\n  column \"emb\" {\n    null = true\n    type = String\n  }\n" +
			"  primary_key {\n    columns = [column.id]\n  }\n  index \"docs_emb\" {\n    type = \"GLOBAL USING vector_kmeans_tree\"\n    columns = [column.emb]\n" +
			"    distance = \"inner_product\"\n    vector_type = \"float\"\n    vector_dimension = 3\n    levels = 2\n    clusters = 4\n  }\n}\n"},
		{file: "schema.yaml", text: "tables:\n  docs:\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      emb: {type: String}\n    indexes:\n" +
			"      docs_emb:\n        type: GLOBAL USING vector_kmeans_tree\n        fields: [emb]\n        distance: inner_product\n        vector_type: float\n" +
			"        vector_dimension: 3\n        levels: 2\n        clusters: 4\n"},
	} {
		t.Run(test.file, func(t *testing.T) {
			c := testcheck.New(t)
			file := filepath.Join(c.TempDir(), test.file)
			c.Assert(os.WriteFile(file, []byte(test.text), 0o600), qt.IsNil)

			out, err := hostCommand(c, "schema", "render", "--schema-file", file, "--dialect", "ydb", "--server-version", "26.2.1.14")

			var exited *exec.ExitError
			c.Assert(err, qt.ErrorAs, &exited, qt.Commentf("%s", out))
			c.Assert(out, qt.Contains, want)
			c.Assert(out, qt.Not(qt.Contains), "inner_product")
		})
	}
}

// A YDB URL that names no database is refused before anything is read, in the
// provider's words for the outcome the built-in engine had. Ported from the
// host's TestReadDBCommand_FailurePath_YDBURLNamesNoDatabase.
func TestHostReadsAURLWithoutADatabase_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	out, err := hostCommand(c, "db", "read", "--db-url", "ydb://localhost:2136")

	var exited *exec.ExitError
	c.Assert(err, qt.ErrorAs, &exited, qt.Commentf("%s", out))
	c.Assert(exited.ExitCode(), qt.Equals, 2)
	c.Assert(out, qt.Contains, "error: provider resolve database context failed (InvalidArgument): YDB URL must name a database")
}
