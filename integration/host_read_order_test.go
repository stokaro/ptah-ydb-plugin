//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"regexp"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// `ptah db read` writes a database whose objects name one another as the
// statements that create them, each after the object it names, as the
// built-in engine writes them: a data source before the external table over
// it, and a resource pool before the classifier that routes to it. The
// provider plans the creations as steps of one contribution, and the host
// orders only the steps of different contributions, so without the
// provider's own order the host refused the read (`invalid schema diff:
// conflicting plan effects: unordered effects on ... resource-pool`).
func TestHostReadsObjectsAfterWhatTheyName_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.DatabaseKindsFeature)
	opened, directory, database := lockFixture(c)
	enableResourcePools(c, opened)
	source, table := path.Join(database, directory, "store"), path.Join(database, directory, "events")
	pool, classifier := directory+"_pool", directory+"_route"
	rank, _ := workloadRanks(directory)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = execFixtureSchema(cleanup, opened.DB, "DROP RESOURCE POOL CLASSIFIER `"+classifier+"`")
		_ = execFixtureSchema(cleanup, opened.DB, "DROP RESOURCE POOL `"+pool+"`")
		_ = execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL TABLE `"+table+"`")
		_ = execFixtureSchema(cleanup, opened.DB, "DROP EXTERNAL DATA SOURCE `"+source+"`")
	})
	for _, statement := range []string{
		"CREATE EXTERNAL DATA SOURCE `" + source + "` WITH (SOURCE_TYPE = 'ObjectStorage', LOCATION = 'https://storage.example.com/bucket/', AUTH_METHOD = 'NONE')",
		"CREATE EXTERNAL TABLE `" + table + "` (id Uint64 NOT NULL, body Utf8) WITH (DATA_SOURCE = '" + source + "', LOCATION = '/data/', FORMAT = 'json_each_row')",
		"CREATE RESOURCE POOL `" + pool + "` WITH (CONCURRENT_QUERY_LIMIT = 10)",
		"CREATE RESOURCE POOL CLASSIFIER `" + classifier + "` WITH (RESOURCE_POOL = '" + pool + "', RANK = " + strconv.FormatInt(rank, 10) +
			", MEMBER_NAME = '" + workloadMember(directory) + "')",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	read, err := hostRunAt(c, os.Getenv("PTAH_YDB_TEST_URL"), "db", "read", "--schemas", directory)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", read))
	c.Assert(read, qt.Matches, `(?s).*CREATE EXTERNAL DATA SOURCE `+regexp.QuoteMeta("`"+directory+"/store`")+
		`.*CREATE EXTERNAL TABLE `+regexp.QuoteMeta("`"+directory+"/events`")+`.*`)
	c.Assert(read, qt.Matches, `(?s).*CREATE RESOURCE POOL `+regexp.QuoteMeta("`"+pool+"`")+
		`.*CREATE RESOURCE POOL CLASSIFIER `+regexp.QuoteMeta("`"+classifier+"`")+`.*`)
}
