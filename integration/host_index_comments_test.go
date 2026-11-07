//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// commentedModels declares users in directory as the host's comment tests do,
// without their view: a comment on the table, on each column, on two indexes
// and on a UNIQUE constraint, which YDB keeps as a unique index.
func commentedModels(directory string) string {
	return "package entities\n\n//ptah:schema:table name=\"users\" schema=\"" + directory + "\" comment=\"People who sign in\"\n" +
		"//ptah:schema:constraint name=\"users_email_key\" type=\"UNIQUE\" columns=\"email\" comment=\"One login each\"\ntype User struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\" comment=\"Key\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"email\" type=\"TEXT\" comment=\"Login\"\n\tEmail string\n" +
		"\t//ptah:schema:field name=\"label\" type=\"TEXT\" comment=\"Shown name\"\n" +
		"\t//ptah:schema:index name=\"users_by_label\" fields=\"label\" comment=\"Lookup by name\"\n\tLabel string\n" +
		"\t//ptah:schema:field name=\"legacy\" type=\"TEXT\" comment=\"Going away\"\n" +
		"\t//ptah:schema:index name=\"users_by_legacy\" fields=\"legacy\" comment=\"Going away too\"\n\tLegacy string\n}\n"
}

// changedModels is commentedModels with every kind of comment change: the
// table's comment rewritten, the email column's removed, the label column's
// rewritten, its index renamed with its comment, the legacy column and its
// index dropped, a commented column and index added, and the UNIQUE
// constraint's comment rewritten.
func changedModels(directory string) string {
	return "package entities\n\n//ptah:schema:table name=\"users\" schema=\"" + directory + "\" comment=\"Customers\"\n" +
		"//ptah:schema:constraint name=\"users_email_key\" type=\"UNIQUE\" columns=\"email\" comment=\"One login per person\"\ntype User struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\" comment=\"Key\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"email\" type=\"TEXT\"\n\tEmail string\n" +
		"\t//ptah:schema:field name=\"label\" type=\"TEXT\" comment=\"Display name\"\n" +
		"\t//ptah:schema:index name=\"users_label_idx\" fields=\"label\" comment=\"Lookup by name\"\n\tLabel string\n" +
		"\t//ptah:schema:field name=\"phone\" type=\"TEXT\" comment=\"Mobile\"\n" +
		"\t//ptah:schema:index name=\"users_by_phone\" fields=\"phone\" comment=\"By phone\"\n\tPhone string\n}\n"
}

// commentedUsersAttributes is what YDB keeps of commentedModels' comments, as
// user attributes of the table under Ptah's keys.
var commentedUsersAttributes = map[string]string{
	"ptah.comment":                       "People who sign in",
	"ptah.comment.column.id":             "Key",
	"ptah.comment.column.email":          "Login",
	"ptah.comment.column.label":          "Shown name",
	"ptah.comment.column.legacy":         "Going away",
	"ptah.comment.index.users_by_label":  "Lookup by name",
	"ptah.comment.index.users_by_legacy": "Going away too",
	"ptah.comment.index.users_email_key": "One login each",
}

// Ported from the host's TestYDBComments_RoundTrip_NothingLeftToPlan, without
// the view, which TestHostKeepsAndChangesAViewComment_HappyPath holds: a table
// created with comments on itself, its columns, two indexes and a UNIQUE
// constraint keeps each as a user attribute of the table under Ptah's key,
// the constraint's under the name of the unique index YDB keeps for it, and
// the next two runs are synced.
func TestHostKeepsTheCommentsOfIndexesAndAUnique_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "users")

	applyUntilSynced(c, directory, "go", commentedModels(directory))

	c.Assert(heldAttributes(c, opened, absolute), qt.DeepEquals, commentedUsersAttributes)
	c.Assert(heldIndexKinds(c, opened, absolute), qt.DeepEquals, map[string]options.IndexType{
		"users_by_label": options.IndexTypeGlobal, "users_by_legacy": options.IndexTypeGlobal, "users_email_key": options.IndexTypeGlobalUnique,
	})
}

// Ported from the host's TestYDBComments_ChangesInPlace, without the view,
// which TestHostKeepsAndChangesAViewComment_HappyPath holds: every kind of
// table, column and index comment change in one plan. The index the
// declaration renames is renamed in place and its comment moves to the new
// name; the comments of the dropped column and of the dropped index are
// removed from the table, which YDB would otherwise keep; the added column
// and the index added on the table that exists get theirs; and the UNIQUE
// constraint's comment is rewritten under its unique index's name.
// Attributes another tool set, one under no key of Ptah's and one under
// Ptah's prefix that names no column or index, are neither read nor removed,
// and nothing is left to plan.
func TestHostChangesTheCommentsOfIndexesInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "users")
	applyUntilSynced(c, directory, "go", commentedModels(directory))
	setStoredAttributes(c, opened, absolute, map[string]string{"owner": "team", "ptah.comment.other": "kept"})

	output, err := hostApply(c, directory, changedModels(directory))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*Schema apply completed successfully.*`)
	// The one index the plan drops is users_by_legacy: users_by_label is
	// renamed, where a removal and an addition would print a second removal.
	c.Assert(unitsSaying(output, "Drops a secondary index"), qt.Equals, 1, qt.Commentf("%s", output))
	output, err = hostApply(c, directory, changedModels(directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
	c.Assert(heldAttributes(c, opened, absolute), qt.DeepEquals, map[string]string{
		"ptah.comment":                       "Customers",
		"ptah.comment.column.id":             "Key",
		"ptah.comment.column.label":          "Display name",
		"ptah.comment.column.phone":          "Mobile",
		"ptah.comment.index.users_label_idx": "Lookup by name",
		"ptah.comment.index.users_by_phone":  "By phone",
		"ptah.comment.index.users_email_key": "One login per person",
		"owner":                              "team",
		"ptah.comment.other":                 "kept",
	})
	c.Assert(heldIndexColumns(c, opened, absolute), qt.DeepEquals, map[string][2][]string{
		"users_label_idx": {{"label"}, nil}, "users_by_phone": {{"phone"}, nil}, "users_email_key": {{"email"}, nil},
	})
}

// Ported from the half of the host's TestYDBComments_ChangesInPlace that
// lints what the plan writes. The host linted the statements its plans wrote
// as two migration files; here `schema diff` prints the statements of each
// plan, the first creating the commented table and the second changing every
// kind of comment, dropping a commented column and a commented index. The
// second removes the comments those leave on the table, so `migrations lint`
// reports no comment left behind (YD150). `schema diff` reads only SQL, HCL,
// YAML and DBML, so the models are YQL here.
func TestHostCommentChangesPassTheLeftCommentRule_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	replacer := strings.NewReplacer("{dir}", directory)
	first := replacer.Replace("CREATE TABLE `{dir}/users` (id Int64 NOT NULL, email Utf8, label Utf8, legacy Utf8, PRIMARY KEY (id), " +
		"INDEX users_email_key GLOBAL UNIQUE ON (email), INDEX users_by_label GLOBAL ON (label), INDEX users_by_legacy GLOBAL ON (legacy));\n" +
		"COMMENT ON TABLE `{dir}/users` IS 'People who sign in'; COMMENT ON COLUMN `{dir}/users`.email IS 'Login'; " +
		"COMMENT ON COLUMN `{dir}/users`.label IS 'Shown name'; COMMENT ON COLUMN `{dir}/users`.legacy IS 'Going away'; " +
		"COMMENT ON INDEX users_email_key ON `{dir}/users` IS 'One login each'; COMMENT ON INDEX users_by_label ON `{dir}/users` IS 'Lookup by name'; " +
		"COMMENT ON INDEX users_by_legacy ON `{dir}/users` IS 'Going away too';\n")
	second := replacer.Replace("CREATE TABLE `{dir}/users` (id Int64 NOT NULL, email Utf8, label Utf8, phone Utf8, PRIMARY KEY (id), " +
		"INDEX users_email_key GLOBAL UNIQUE ON (email), INDEX users_by_label GLOBAL ON (label), INDEX users_by_phone GLOBAL ON (phone));\n" +
		"COMMENT ON TABLE `{dir}/users` IS 'Customers'; COMMENT ON COLUMN `{dir}/users`.label IS 'Display name'; " +
		"COMMENT ON COLUMN `{dir}/users`.phone IS 'Mobile'; COMMENT ON INDEX users_email_key ON `{dir}/users` IS 'One login per person'; " +
		"COMMENT ON INDEX users_by_label ON `{dir}/users` IS 'Lookup by name'; COMMENT ON INDEX users_by_phone ON `{dir}/users` IS 'By phone';\n")
	sources := writeFiles(c, c.TempDir(), map[string]string{"first.sql": first, "second.sql": second})
	migrations := c.TempDir()
	diff := func(name, version string) string {
		c.Helper()
		statements, err := hostCommand(c, "schema", "diff", "--from", os.Getenv("PTAH_YDB_TEST_URL"), "--to", filepath.Join(sources, name+".sql"), "--schemas", directory)
		c.Assert(err, qt.IsNil, qt.Commentf("schema diff %s:\n%s", name, statements))
		c.Assert(os.WriteFile(filepath.Join(migrations, version+"_"+name+".up.sql"), []byte(statements), 0o600), qt.IsNil) // #nosec G703 -- a fixed file name under the test's c.TempDir()
		return statements
	}

	diff("first", "0000000001")
	applyRuns(c, directory, "sql", first)
	planned := diff("second", "0000000002")
	lint, err := hostCommand(c, "migrations", "lint", "--dialect", "ydb", "--dir", migrations, "--fail-on", "none")

	c.Assert(err, qt.IsNil, qt.Commentf("migrations lint:\n%s", lint))
	c.Assert(lint, qt.Not(qt.Contains), "YD150", qt.Commentf("%s\n%s", planned, lint))
	c.Assert(planned, qt.Contains, "COMMENT ON COLUMN `"+directory+"/users`.`legacy` IS NULL", qt.Commentf("%s", planned))
	c.Assert(planned, qt.Contains, "COMMENT ON INDEX `users_by_legacy` ON `"+directory+"/users` IS NULL", qt.Commentf("%s", planned))
}

// Ported from the host's TestYDBComments_IntrospectedModelsPlanNothing:
// `ptah introspect` writes the commented table and the commented view over it
// as Go models, the comments of the table, its columns, its indexes, the
// UNIQUE constraint's unique index and the view reach the annotations, and the
// models, applied over the directory they came from, plan nothing.
func TestHostIntrospectedCommentsPlanNothing_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	view := "\n//ptah:schema:view name=\"active_users\" schema=\"" + directory + "\" body=\"SELECT id FROM `" + directory + "/users`\" comment=\"Users who signed in\"\n" +
		"type ActiveUsers struct{}\n"
	applyUntilSynced(c, directory, "go", commentedModels(directory)+view)

	models := introspectDirectory(c, directory)

	for _, comment := range []string{"People who sign in", "Key", "Login", "Shown name", "Going away", "Lookup by name", "Going away too", "One login each", "Users who signed in"} {
		c.Assert(models, qt.Contains, `comment="`+comment+`"`, qt.Commentf("%s", models))
	}
	output, err := hostApplySelected(c, directory, "go", models)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", models, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", models, output))
	c.Assert(heldAttributes(c, opened, path.Join(database, directory, "users")), qt.DeepEquals, commentedUsersAttributes)
	c.Assert(storedAttributes(c, opened, path.Join(database, directory, "active_users")), qt.DeepEquals, map[string]string{"ptah.comment": "Users who signed in"})
}
