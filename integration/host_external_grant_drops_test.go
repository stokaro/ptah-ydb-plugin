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
)

// Ported from the external data source and external table rows of the host's
// TestGenerateMigrationAST_ObjectPermissions_GoWithADroppedObject
// (internal/planner/dialects/ydb): a source that drops an external table and
// its data source while the user holds a permission on each revokes neither
// entry, since a REVOKE after the drop would name an object that is gone. The
// plan holds the two drops alone, as the built-in engine at 74de0b65b planned
// them on 25.1.4.7 and 26.2.1.14; it applies, and nothing is left to plan.
func TestHostDropsExternalObjectsHoldingAGrant_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := objectAccessFixture(c)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(ctx, f.opened.DB, "DROP EXTERNAL TABLE `"+path.Join(f.database, f.directory, "files")+"`")
		_ = execFixtureSchema(ctx, f.opened.DB, "DROP EXTERNAL DATA SOURCE `"+path.Join(f.database, f.directory, "bucket")+"`")
	})
	declared := "external_data_sources:\n  bucket:\n    schema: " + f.directory + "\n    source_type: ObjectStorage\n    location: https://storage.example.test/events/\n    auth_method: NONE\n" +
		"external_tables:\n  files:\n    schema: " + f.directory + "\n    data_source: " + f.directory + "/bucket\n    location: 2026/\n    columns:\n" +
		"      - {name: id, type: Int64, not_null: true}\n      - {name: kind, type: Utf8}\n    options:\n      FORMAT: csv_with_names\n" +
		f.document(nil, "") +
		"  bucket:\n    role: " + f.names.user + "\n    privileges: [SELECT ROW]\n    on_external_data_source: " + f.directory + ".bucket\n" +
		"  files:\n    role: " + f.names.user + "\n    privileges: [SELECT ROW]\n    on_external_table: " + f.directory + ".files\n"
	emptied := "external_data_sources: {}\nexternal_tables: {}\n" + f.document(nil, "")
	hostRun(c, f.directory, "the objects", declared, `(?s).*Schema apply completed successfully.*`)
	held := grantsIn(c, f.client, f.session, f.directory, []string{"external data source", "external table"}, []string{f.names.user})

	drops := hostPlanned(c, f.directory, "yaml", emptied)
	hostRun(c, f.directory, "the drops", emptied, `(?s).*Schema apply completed successfully.*`)

	c.Assert(held, qt.HasLen, 2)
	c.Assert(drops, qt.DeepEquals, []string{"external-table-operation " + f.directory + ".files", "external-data-source-operation " + f.directory + ".bucket"})
	c.Assert(strings.Join(drops, "\n"), qt.Not(qt.Contains), "REVOKE")
	c.Assert(hostPlanned(c, f.directory, "yaml", emptied), qt.HasLen, 0)
	c.Assert(listedNames(c, f.opened, path.Join(f.database, f.directory)), qt.HasLen, 0)
}
