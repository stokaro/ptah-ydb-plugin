//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A file-to-file comparison needs the database root of the YDB dev URL to
// resolve a grant on the database and one on its shop directory. Neither
// side reads a live catalog, so the root must reach the provider's source
// context. Ported from the host's TestYQLDocumentDiffUsesDeclaredDatabaseRoot.
func TestHostFileDiffUsesYDBDatabaseRootForGrants_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	user := principalName("ptahroot")
	from := "CREATE TABLE `shop/orders` (id Int64 NOT NULL, PRIMARY KEY (id)); CREATE USER `" + user + "`;\n"
	to := from + "GRANT CONNECT ON `/local` TO `" + user + "`; GRANT LIST ON `/local/shop` TO `" + user + "`;\n"
	files := writeFiles(c, c.TempDir(), map[string]string{"from.sql": from, "to.sql": to})

	out, err := hostCommand(c, "schema", "diff", "--from", filepath.Join(files, "from.sql"), "--to", filepath.Join(files, "to.sql"),
		"--dev-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", "shop")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	statements := previewStatements(out)
	c.Assert(statements, qt.HasLen, 2)
	c.Assert(statements, qt.Contains, "GRANT 'ydb.database.connect' ON `/local` TO `"+user+"`;")
	// YDB 25.1 renders the directory's absolute path; 26.2 renders it
	// relative to /local. Both name the same directory in this session.
	c.Assert(strings.Join(statements, "\n"), qt.Matches,
		"(?s).*GRANT 'ydb.generic.list' ON `(?:/local/)?shop` TO `"+user+"`;.*")
}
