//go:build integration

package integration_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// lineageEdge is one edge of `ptah schema lineage --format json`.
type lineageEdge struct {
	FromTable  string `json:"from_table"`
	FromColumn string `json:"from_column"`
	ToView     string `json:"to_view"`
	ToColumn   string `json:"to_column"`
}

// lineageShop is a YQL schema file: a table in the directory shop, and views
// over it by its path. One view holds a double-quoted string and one an @@
// string with a quote inside, which the host's shared rules misread.
const lineageShop = "CREATE TABLE `shop/items` (\n    id Int64 NOT NULL,\n    label Utf8,\n    qty Int64,\n    PRIMARY KEY (id)\n);\n" +
	"CREATE VIEW `shop/active` WITH (security_invoker = TRUE) AS SELECT id, label AS name FROM `shop/items` WHERE qty > 0;\n" +
	"CREATE VIEW `shop/active_names` WITH (security_invoker = TRUE) AS SELECT name FROM `shop/active`;\n" +
	"CREATE VIEW `shop/everything` WITH (security_invoker = TRUE) AS SELECT * FROM `shop/items`;\n" +
	"CREATE VIEW `shop/tagged` WITH (security_invoker = TRUE) AS SELECT id, \"fixed\" AS tag FROM `shop/items`;\n" +
	"CREATE VIEW `shop/noted` WITH (security_invoker = TRUE) AS SELECT id, @@it's@@ AS note, label FROM `shop/items`;\n"

// The host traces YQL views with this provider's scanner (sql.scan): each
// source is the table at its path, a star resolves to the table's columns, and
// a string feeds no column. These are the edges the built-in YDB engine
// traced. The lineage is read offline, from the file alone.
func TestHostTracesTheLineageOfYQLViews_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	schemaFile := filepath.Join(writeFiles(c, c.TempDir(), map[string]string{"shop.sql": lineageShop}), "shop.sql")

	out, err := hostCommand(c, "schema", "lineage", "--schema-file", schemaFile, "--dialect", "ydb", "--format", "json")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	var document struct {
		Edges     []lineageEdge     `json:"edges"`
		Undecided []json.RawMessage `json:"undecided"`
	}
	c.Assert(json.Unmarshal([]byte(out), &document), qt.IsNil, qt.Commentf("%s", out))
	c.Assert(document.Undecided, qt.HasLen, 0)
	c.Assert(document.Edges, qt.DeepEquals, []lineageEdge{
		{FromTable: "shop.items", FromColumn: "id", ToView: "shop.active", ToColumn: "id"},
		{FromTable: "shop.items", FromColumn: "label", ToView: "shop.active", ToColumn: "name"},
		{FromTable: "shop.active", FromColumn: "name", ToView: "shop.active_names", ToColumn: "name"},
		{FromTable: "shop.items", FromColumn: "id", ToView: "shop.everything", ToColumn: "id"},
		{FromTable: "shop.items", FromColumn: "label", ToView: "shop.everything", ToColumn: "label"},
		{FromTable: "shop.items", FromColumn: "qty", ToView: "shop.everything", ToColumn: "qty"},
		{FromTable: "shop.items", FromColumn: "id", ToView: "shop.noted", ToColumn: "id"},
		{FromTable: "shop.items", FromColumn: "label", ToView: "shop.noted", ToColumn: "label"},
		{FromTable: "shop.items", FromColumn: "id", ToView: "shop.tagged", ToColumn: "id"},
	})
}
