//go:build integration

package integration_test

import (
	"context"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A scoped `schema apply` drops an external data source its directory no
// longer declares, which an external table in another directory reads. YDB
// refuses the DROP with SCHEME_ERROR before it changes anything, and the host
// reports the refusal with the table YDB names, as the built-in engine at host
// 74de0b65b did on 25.1.4.7 and 26.2.1.14 (`Other entities depend on this data
// source, please remove them at the beginning: /local/<dir>/tbl`). The
// provider reported the unit's outcome as unknown (gap G134).
func TestHostRefusesADropOthersDependOn_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	readers := directory + "_readers"
	source, table := path.Join(database, directory, "src"), path.Join(database, readers, "tbl")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE EXTERNAL DATA SOURCE `"+source+
		"` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.example.test/g134/', AUTH_METHOD = 'NONE')"), qt.IsNil)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE EXTERNAL TABLE `"+table+
		"` (id Int64 NOT NULL) WITH (DATA_SOURCE = '"+source+"', LOCATION = '/', FORMAT = 'json_each_row')"), qt.IsNil)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL TABLE `"+table+"`"), qt.IsNil)
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL DATA SOURCE `"+source+"`"), qt.IsNil)
		c.Check(opened.Driver.Scheme().RemoveDirectory(cleanup, path.Join(database, readers)), qt.IsNil)
	})

	output, err := hostApplySource(c, directory, "yaml", "external_data_sources: {}\nexternal_tables: {}\n")

	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "NOT_APPLIED")
	c.Assert(output, qt.Contains, "YDB refused the statement: other scheme objects depend on what it drops, "+table+
		"; drop them first (SCHEME_ERROR, issue 2019)")
	c.Assert(output, qt.Not(qt.Contains), "outcome is unknown")
	c.Assert(describeSource(c, opened, source).SourceType, qt.Equals, "ObjectStorage")
}
