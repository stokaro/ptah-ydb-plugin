//go:build integration

package integration_test

import (
	"path"
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

// Ported from the host's TestYDBComments_IntrospectedModelsPlanNothing, for
// the table and its indexes: `ptah introspect` writes the commented table as
// Go models, the comments of the table, its columns, its indexes and the
// UNIQUE constraint's unique index reach the annotations, and the models,
// applied over the directory they came from, plan nothing.
func TestHostIntrospectedCommentsPlanNothing_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	applyUntilSynced(c, directory, "go", commentedModels(directory))

	models := introspectDirectory(c, directory)

	for _, comment := range []string{"People who sign in", "Key", "Login", "Shown name", "Going away", "Lookup by name", "Going away too", "One login each"} {
		c.Assert(models, qt.Contains, `comment="`+comment+`"`, qt.Commentf("%s", models))
	}
	output, err := hostApplySelected(c, directory, "go", models)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", models, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", models, output))
	c.Assert(heldAttributes(c, opened, path.Join(database, directory, "users")), qt.DeepEquals, commentedUsersAttributes)
}
