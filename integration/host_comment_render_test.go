//go:build integration

package integration_test

import (
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A complete authored schema renders every comment after its object exists.
// The UNIQUE constraint's comment belongs to the YDB index that holds it.
// This covers the creation cases of the former host's
// TestRender_Comments_HappyPath through the host binary.
func TestHostRendersCommentsAfterTheirObjects_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostChange(c, "stokaro/ptah#5736, which renders authored table and index comments")
	view := "package entities\n\n//ptah:schema:view name=\"active\" schema=\"shop\" body=\"SELECT id FROM `shop/users`\" " +
		"comment=\"Recent\"\ntype Active struct{}\n"
	dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{
		"users.go": commentedModels("shop"),
		"view.go":  view,
	})
	for _, version := range []string{"25.1.4.7", "26.2.1.14"} {
		t.Run(version, func(t *testing.T) {
			c := testcheck.New(t)
			out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb", "--server-version", version)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			for _, statement := range []string{
				"COMMENT ON TABLE `shop/users` IS 'People who sign in';",
				"COMMENT ON COLUMN `shop/users`.`email` IS 'Login';",
				"COMMENT ON INDEX `users_by_label` ON `shop/users` IS 'Lookup by name';",
				"COMMENT ON INDEX `users_email_key` ON `shop/users` IS 'One login each';",
				"COMMENT ON VIEW `shop/active` IS 'Recent';",
			} {
				c.Assert(out, qt.Contains, statement)
			}
			for _, pair := range [][2]string{
				{"CREATE TABLE `shop/users`", "COMMENT ON TABLE `shop/users`"},
				{"CREATE TABLE `shop/users`", "COMMENT ON COLUMN `shop/users`.`email`"},
				{"CREATE TABLE `shop/users`", "COMMENT ON INDEX `users_by_label`"},
				{"CREATE TABLE `shop/users`", "COMMENT ON INDEX `users_email_key`"},
				{"CREATE VIEW `shop/active`", "COMMENT ON VIEW `shop/active`"},
			} {
				c.Assert(out, qt.Contains, pair[0], qt.Commentf("missing %q from:\n%s", pair[0], out))
				c.Assert(strings.Index(out, pair[0]) < strings.Index(out, pair[1]), qt.IsTrue,
					qt.Commentf("%q must precede %q:\n%s", pair[0], pair[1], out))
			}
		})
	}
}
