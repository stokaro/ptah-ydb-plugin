//go:build integration

package integration_test

import (
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// selfDiffSchema declares, in YQL, a table with a starting layout and read
// replicas, and one with a TTL.
const selfDiffSchema = "CREATE TABLE `app/events` (id Uint64 NOT NULL, PRIMARY KEY (id)) WITH (UNIFORM_PARTITIONS = 4, READ_REPLICAS_SETTINGS = \"PER_AZ:1\");\n" +
	"CREATE TABLE `app/sessions` (id Uint64 NOT NULL, expire_at Timestamp, PRIMARY KEY (id)) WITH (TTL = Interval(\"P30D\") ON expire_at);\n"

// A YQL schema diffed against itself plans nothing: the host reads the file
// as the declaration on one side and converts the same declaration through
// this provider into what a database would hold on the other, so every
// setting the document states survives the conversion: a starting layout,
// which YDB keeps only as the minimum partition count it sets, read replicas
// and a TTL. A dev realm pins the dialect and holds nothing. Ported from the
// host's TestCompare_YDBTablePartitioning_SameDocument and
// TestCompare_YDBTTL_HappyPath.
func TestHostDiffsAYQLSchemaAgainstItself_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	file := filepath.Join(writeFiles(c, c.TempDir(), map[string]string{"schema.sql": selfDiffSchema}), "schema.sql")

	out, err := hostCommand(c, "schema", "diff", "--from", file, "--to", file, "--dev-url", realm.address)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Equals, "Schemas are synced, no changes to be made.\n")
}
