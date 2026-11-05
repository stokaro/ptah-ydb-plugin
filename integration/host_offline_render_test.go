//go:build integration

package integration_test

import (
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
