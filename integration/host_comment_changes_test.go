//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
)

// storedAttributes reads the user attributes YDB keeps on the table or view
// at absolute, where Ptah's comments live.
func storedAttributes(c *qt.C, opened *connection.Connection, absolute string) map[string]string {
	c.Helper()
	var attributes map[string]string
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, absolute)
		if err != nil {
			return err
		}
		attributes = described.Attributes
		return nil
	}, table.WithIdempotent())
	c.Assert(err, qt.IsNil)
	if attributes == nil {
		attributes = map[string]string{}
	}
	return attributes
}

// commentedUsers is table users of the fixture directory in YAML: the
// columns given, each a nullable Utf8 column beside the key, with the comments
// given by column name and the table's comment.
func commentedUsers(directory, tableComment string, columns []string, comments map[string]string) string {
	var text strings.Builder
	text.WriteString("tables:\n  users:\n    schema: " + directory + "\n")
	if tableComment != "" {
		text.WriteString("    comment: " + strconv.Quote(tableComment) + "\n")
	}
	text.WriteString("    columns:\n")
	for _, name := range append([]string{"id"}, columns...) {
		column := "{type: Utf8"
		if name == "id" {
			column = "{type: Int64, primary: true, not_null: true"
		}
		if comment, ok := comments[name]; ok {
			column += ", comment: " + strconv.Quote(comment)
		}
		text.WriteString("      " + name + ": " + column + "}\n")
	}
	return text.String()
}

// applyRuns applies source through the host and requires that the run
// changes the database and the next one finds nothing to change.
func applyRuns(c *qt.C, selectors, format, source string) {
	c.Helper()
	for _, run := range []struct{ name, want string }{
		{"the run", `(?s).*Schema apply completed successfully.*`},
		{"the next run", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApplySelected(c, selectors, format, source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s\n%s", run.name, source, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s: %s\n%s", run.name, source, output))
	}
}

// viewWithComment declares table users of the fixture directory and a view
// over it with the comment given.
func viewWithComment(directory, comment string) string {
	return "package entities\n\n//ptah:schema:table name=\"users\" schema=\"" + directory + "\"\ntype User struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n}\n\n" +
		"//ptah:schema:view name=\"active_users\" schema=\"" + directory + "\" body=\"SELECT id FROM `" + directory + "/users`\" comment=\"" + comment + "\"\n" +
		"type ActiveUsers struct{}\n"
}

// The view half of the host's TestYDBComments_RoundTrip_NothingLeftToPlan and
// TestYDBComments_ChangesInPlace: a view created with a comment keeps it under
// Ptah's key, a changed comment is written in place, and nothing is left to
// plan after either.
func TestHostKeepsAndChangesAViewComment_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	viewsAtCleanup(c, opened, database, directory)
	view := path.Join(database, directory, "active_users")

	applyUntilSynced(c, directory, "go", viewWithComment(directory, "Users who signed in"))
	c.Assert(storedAttributes(c, opened, view), qt.DeepEquals, map[string]string{"ptah.comment": "Users who signed in"})

	applyRuns(c, directory, "go", viewWithComment(directory, "Signed in lately"))
	c.Assert(storedAttributes(c, opened, view), qt.DeepEquals, map[string]string{"ptah.comment": "Signed in lately"})
}

// commentTexts are comments holding what a string literal has to escape, text
// beyond ASCII, a NUL, and the longest value YDB keeps, by the column that
// carries each.
var commentTexts = map[string]string{
	"quotes":    `it's "quoted" and ` + "`ticked`",
	"escapes":   `a \ backslash, a \n written out`,
	"controls":  "a newline\nand a tab\tand a NUL \x00 here",
	"non_ascii": "текст и 漢字",
	"longest":   strings.Repeat("x", 4096),
}

// Ported from the host's TestYDBComments_TextRoundTrips: comments holding what
// a string literal has to escape, text beyond ASCII, a NUL and the longest
// value YDB keeps read back byte for byte, and nothing is left to plan. YAML
// spells each of them, the NUL as \0. The table is created without its
// comments first, since the provider path of the pinned host leaves the
// comments of a created table out (stokaro/ptah#5256).
func TestHostCommentTextRoundTrips_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	columns := []string{"quotes", "escapes", "controls", "non_ascii", "longest"}
	applyRuns(c, directory, "yaml", commentedUsers(directory, "", columns, nil))

	applyRuns(c, directory, "yaml", strings.ReplaceAll(commentedUsers(directory, "Notes", columns, commentTexts), `\x00`, `\0`))

	held := storedAttributes(c, opened, path.Join(database, directory, "users"))
	for name, text := range commentTexts {
		c.Assert(held["ptah.comment.column."+name], qt.Equals, text, qt.Commentf("column %s", name))
	}
}

// Ported from the host's TestYDBComments_MigrationFile: a hand-written
// migration that comments a table and a column runs through `ptah
// migrations up` and the provider. Each COMMENT ON is a scheme query of its
// own, counted and recorded like any other, so the revision records four of
// four, and the comments read back.
func TestHostRunsCommentsOfAMigrationFile_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	migrations := c.TempDir()
	notes := "`" + directory + "/notes`"
	c.Assert(os.WriteFile(filepath.Join(migrations, "0000000001_notes.up.sql"), []byte("CREATE TABLE "+notes+" (id Int64 NOT NULL, body Utf8, PRIMARY KEY (id));\n"+
		"COMMENT ON TABLE "+notes+" IS 'Notes';\nUPSERT INTO "+notes+" (id, body) VALUES (1l, 'first'u);\nCOMMENT ON COLUMN "+notes+".`body` IS 'The text';\n"), 0o600), qt.IsNil)
	c.Assert(os.WriteFile(filepath.Join(migrations, "0000000001_notes.down.sql"), []byte("DROP TABLE "+notes+";\n"), 0o600), qt.IsNil)

	output, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "migrations", "up", "--migrations-dir", migrations, "--migrations-schema", directory)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	binary, digest := providerBinary(c)
	client, session, _ := historySession(c, binary, digest)
	read := historyRead(c, client, &protocol.HistoryScope{SessionId: session, Schema: directory, Table: "schema_migrations", Format: protocol.HistoryScope_NATIVE})
	c.Assert(read.Revisions, qt.HasLen, 1)
	c.Assert([]any{read.Revisions[0].Version, read.Revisions[0].Applied, read.Revisions[0].Total}, qt.DeepEquals, []any{"1", uint64(4), uint64(4)})
	c.Assert(storedAttributes(c, opened, path.Join(database, directory, "notes")), qt.DeepEquals, map[string]string{"ptah.comment": "Notes", "ptah.comment.column.body": "The text"})
	c.Assert(rowCount(c, opened, path.Join(database, directory, "notes"), ""), qt.Equals, int64(1))
}

