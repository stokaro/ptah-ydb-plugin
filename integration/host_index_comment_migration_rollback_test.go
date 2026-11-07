//go:build integration

package integration_test

import (
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func indexCommentSource(directory, index, comment, otherComment string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"label\" type=\"Utf8\"\n" +
		"\t//ptah:schema:index name=\"" + index + "\" fields=\"label\" comment=\"" + comment + "\"\n\tLabel string\n" +
		"\t//ptah:schema:field name=\"category\" type=\"Utf8\"\n" +
		"\t//ptah:schema:index name=\"by_category\" fields=\"category\" comment=\"" + otherComment + "\"\n\tCategory string\n}\n"
}

// The down file of an index rename restores its old name and comment under
// that name. It clears the comment under the new name, which YDB otherwise
// keeps as a table attribute, and restores a comment removed from a second
// index. The server and the host's next plan agree in both directions. This
// ports the index-comment half of
// TestPlanBidirectionalSchemaDiff_IndexCommentsRollBack.
func TestHostRollsBackARenamedIndexComment_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	absolute := path.Join(database, directory, "items")
	before := indexCommentSource(directory, "by_label", "Old", "Kept")
	after := indexCommentSource(directory, "label_idx", "New", "")
	created, createErr := hostApply(c, directory, before)
	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))

	migrations, _, down := hostGenerate(c, directory, "go", after)
	applied, upErr := hostMigrate(c, directory, migrations, "migrations", "up")
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(heldAttributes(c, opened, absolute), qt.DeepEquals, map[string]string{"ptah.comment.index.label_idx": "New"})
	c.Assert(hostPlanned(c, directory, "go", after), qt.HasLen, 0)

	rolledBack, downErr := hostMigrate(c, directory, migrations, "migrations", "down", "--target", "0", "--confirm")
	c.Assert(downErr, qt.IsNil, qt.Commentf("%s\n%s", rolledBack, down))
	c.Assert(down, qt.Contains, "ALTER TABLE `"+directory+"/items` RENAME INDEX `label_idx` TO `by_label`;")
	c.Assert(down, qt.Contains, "COMMENT ON INDEX `by_label` ON `"+directory+"/items` IS 'Old';")
	c.Assert(down, qt.Contains, "COMMENT ON INDEX `label_idx` ON `"+directory+"/items` IS NULL;")
	c.Assert(down, qt.Contains, "COMMENT ON INDEX `by_category` ON `"+directory+"/items` IS 'Kept';")
	c.Assert(strings.Index(down, "RENAME INDEX `label_idx` TO `by_label`") < strings.Index(down, "COMMENT ON INDEX `by_label`"), qt.IsTrue, qt.Commentf("%s", down))
	c.Assert(heldAttributes(c, opened, absolute), qt.DeepEquals, map[string]string{"ptah.comment.index.by_label": "Old", "ptah.comment.index.by_category": "Kept"})
	c.Assert(hostPlanned(c, directory, "go", before), qt.HasLen, 0)
}
