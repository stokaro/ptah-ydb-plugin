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

// The agent's render_schema YDB case has one table in a directory and a
// VARCHAR declaration whose YQL spelling is Utf8. The CLI reaches the same
// host renderer through this provider. Ported from TestRenderSchema_YDB.
func TestHostRendersTheAgentYDBTable_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, c.TempDir(), map[string]string{"ledger.go": `package models

//ptah:schema:table name="entries" schema="books"
type Entry struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="memo" type="VARCHAR(255)" not_null="true"
	Memo string
}
`})

	out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb", "--server-version", "25.1.4.7")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(strings.Count(out, "CREATE TABLE"), qt.Equals, 1)
	c.Assert(out, qt.Contains, "CREATE TABLE `books/entries` (")
	c.Assert(out, qt.Contains, "`memo` Utf8 NOT NULL,")
}

// The agent's YDB view case requires the YDB security clause on every view.
// Ported from TestRenderSchema_YDBView.
func TestHostRendersTheAgentYDBView_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, c.TempDir(), map[string]string{"bookshop.go": `package models

//ptah:schema:table name="authors"
type Author struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="name" type="TEXT" not_null="true"
	Name string
}

//ptah:schema:view name="author_names" body="SELECT name FROM authors"
type AuthorNames struct{}
`})

	out, err := hostCommand(c, "schema", "render", "--root-dir", dir, "--dialect", "ydb", "--server-version", "25.1.4.7")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "CREATE VIEW `author_names` WITH (security_invoker = TRUE) AS\nSELECT name FROM authors\n;")
}

// The agent's validate_schema case reports a TIME column as having no YDB
// counterpart. The CLI asks this provider to validate the same Go source.
// Ported from TestValidateSchema_YDB.
func TestHostValidatesTheAgentYDBTimeColumn_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	file := filepath.Join(c.TempDir(), "entities.go")
	c.Assert(os.WriteFile(file, []byte(`package models

//ptah:schema:table name="entries"
type Entry struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="at" type="TIME"
	At string
}
`), 0o600), qt.IsNil)

	out, err := hostCommand(c, "schema", "validate", "--root-dir", filepath.Dir(file), "--dialect", "ydb", "--server-version", "25.1.4.7")

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, `column "at" of table "entries": TIME has no YDB counterpart: YDB has no time-of-day type`)
	c.Assert(out, qt.Contains, "1 structural problem")
}
