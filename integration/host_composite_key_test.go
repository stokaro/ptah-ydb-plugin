//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// The host creates a table whose source marks two columns primary and
// orders no key, from YAML and from Go: the key is the marked columns in the
// order the source declares them, as the host's embedded engine creates it
// (measured with host v0.19.0: `PRIMARY KEY (partition, offset)`), and the
// next run plans nothing. Without the order, the provider refused the creation
// (`provider project table creations failed (InvalidArgument)`).
func TestHostKeysATableByTheColumnsItMarksPrimary_HappyPath(t *testing.T) {
	for _, test := range []struct{ format, source string }{
		{format: "yaml", source: "tables:\n  order_log:\n    schema: {dir}\n    columns:\n      partition: {type: Uint32, primary: true, not_null: true}\n" +
			"      offset: {type: Uint64, primary: true, not_null: true}\n      message: {type: Utf8}\n"},
		{format: "go", source: "package entities\n\n//ptah:schema:table name=\"order_log\" schema=\"{dir}\"\ntype OrderLog struct {\n" +
			"\t//ptah:schema:field name=\"partition\" type=\"Uint32\" primary=\"true\" not_null=\"true\"\n\tPartition uint32\n" +
			"\t//ptah:schema:field name=\"offset\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tOffset uint64\n" +
			"\t//ptah:schema:field name=\"message\" type=\"Utf8\"\n\tMessage string\n}\n"},
	} {
		t.Run(test.format, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, "order_log")
			c.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
			})
			source := strings.ReplaceAll(test.source, "{dir}", directory)

			created, createdErr := hostApplySource(c, directory, test.format, source)
			settled, settledErr := hostApplySource(c, directory, test.format, source)

			c.Assert(createdErr, qt.IsNil, qt.Commentf("%s", created))
			c.Assert(created, qt.Contains, "Schema apply completed successfully")
			c.Assert(settledErr, qt.IsNil, qt.Commentf("%s", settled))
			c.Assert(settled, qt.Contains, "Schema is synced, no changes to be made")
			var described options.Description
			c.Assert(opened.Driver.Table().Do(c.Context(), func(ctx context.Context, s table.Session) error {
				var err error
				described, err = s.DescribeTable(ctx, absolute)
				return err
			}), qt.IsNil)
			c.Assert(described.PrimaryKey, qt.DeepEquals, []string{"partition", "offset"})
		})
	}
}
