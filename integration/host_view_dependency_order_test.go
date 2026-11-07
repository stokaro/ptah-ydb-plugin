//go:build integration

package integration_test

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A YDB view refers to another by its path. YQL strings and quoted names do
// not create dependencies. This ports the YDB cases of the former host's
// TestViewLikesForCreateForDialect through authored schema rendering.
func TestHostRendersYDBViewsInDependencyOrder_HappyPath(t *testing.T) {
	requireHostChange(testcheck.New(t), "stokaro/ptah#5740, which orders views by what the provider's SQL scanner reads")
	for _, test := range []struct {
		name, reference, baseBody string
	}{
		{"path reference", "`shop/z_base`", "SELECT 1 AS id"},
		{"unqualified reference", "z_base", "SELECT 1 AS id"},
		{"double-quoted string", "`shop/z_base`", "SELECT \"`shop/a_report`\" AS label"},
		{"escaped quote in double-quoted string", "`shop/z_base`", "SELECT \"x\\\" `shop/a_report`\" AS label"},
		{"escaped quote in single-quoted string", "`shop/z_base`", "SELECT 'it\\'s `shop/a_report`' AS label"},
		{"multiline string", "`shop/z_base`", "SELECT @@`shop/a_report`@@ AS label"},
		{"escaped backtick in name", "`shop/z_base`", "SELECT 1 AS `odd\\` a_report`"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			source := "package entities\n\n" +
				"//ptah:schema:view name=\"a_report\" schema=\"shop\" body=" + strconv.Quote("SELECT id FROM "+test.reference) + "\n" +
				"type Report struct{}\n\n" +
				"//ptah:schema:view name=\"z_base\" schema=\"shop\" body=" + strconv.Quote(test.baseBody) + "\n" +
				"type Base struct{}\n"
			dir := writeFiles(c, filepath.Join(c.TempDir(), "entities"), map[string]string{"views.go": source})
			out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb", "--server-version", "26.2.1.14")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			base := "CREATE VIEW `shop/z_base`"
			report := "CREATE VIEW `shop/a_report`"
			c.Assert(out, qt.Contains, base)
			c.Assert(out, qt.Contains, report)
			c.Assert(strings.Index(out, base) < strings.Index(out, report), qt.IsTrue, qt.Commentf("%s", out))
		})
	}
}
