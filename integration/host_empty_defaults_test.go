//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// storedDefaults reads the literal default of each column of the table at
// absolute back from the server, by column name, for the columns that have
// one.
func storedDefaults(c *qt.C, opened *connection.Connection, absolute string) map[string]string {
	c.Helper()
	defaults := make(map[string]string)
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, absolute)
		if err != nil {
			return err
		}
		for _, column := range described.Columns {
			if column.DefaultValue != nil && column.DefaultValue.Literal() != nil {
				defaults[column.Name] = column.DefaultValue.Literal().Yql()
			}
		}
		return nil
	}, table.WithIdempotent())
	c.Assert(err, qt.IsNil)
	return defaults
}

// Ported from the host's TestYDBEmptyDefaults_ExportRecreatesTheStoredDefaults:
// the empty defaults of a Utf8 and a String column are read as present
// values, `ptah introspect` exports them, the export plans nothing against
// the table it came from, and the table created again from it, after the
// first one is dropped, holds both defaults and plans nothing.
//
// The host's engine creates the table again with YQL, which stores the plain
// literal it was created with. The provider creates it through the table
// service, which stores the default of a nullable column as an optional
// literal, Just(""u): the same value, which the provider reads as the same
// default, so the plan converges. The raw forms are pinned on both sides.
func TestHostExportRecreatesTheStoredEmptyDefaults_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	items := path.Join(database, directory, "items")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+items+"` (id Int64 NOT NULL, text_value Utf8 DEFAULT ''u, bytes_value String DEFAULT '', PRIMARY KEY (id))"), qt.IsNil)
	before := storedDefaults(c, opened, items)

	models := introspectDirectory(c, directory)
	c.Assert(hostPlanned(c, directory, "go", models), qt.HasLen, 0, qt.Commentf("%s", models))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "DROP TABLE `"+items+"`"), qt.IsNil)
	applyUntilSynced(c, directory, "go", models)

	c.Assert(before, qt.DeepEquals, map[string]string{"text_value": `""u`, "bytes_value": `""`})
	c.Assert(storedDefaults(c, opened, items), qt.DeepEquals, map[string]string{"text_value": `Just(""u)`, "bytes_value": `Just("")`})
}
