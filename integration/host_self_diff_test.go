//go:build integration

package integration_test

import (
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
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

// The host's TestCompare_YDBUniqueConstraintIsItsIndex and
// TestCompareSchemas_YDBSelfCompareReportsNothing exercise the file-to-file
// path. Every declared global index must be described as the provider would
// prepare it on both sides, including the unique index and the asynchronous
// index with covered columns. Otherwise an unchanged document plans index
// drops and additions, or refuses to add the unique index on older servers.
func TestHostDiffsYQLIndexesAgainstThemselves_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	const schema = "CREATE TABLE `accounts` (id Int64 NOT NULL, email Utf8, login Utf8, balance Decimal(22,9), nickname Utf8, PRIMARY KEY (id), " +
		"INDEX by_login GLOBAL ON (login), INDEX by_nickname GLOBAL SYNC ON (nickname), " +
		"INDEX by_balance GLOBAL ASYNC ON (balance) COVER (nickname), " +
		"INDEX accounts_email_key GLOBAL UNIQUE SYNC ON (email));\n"
	file := filepath.Join(writeFiles(c, c.TempDir(), map[string]string{"schema.sql": schema}), "schema.sql")

	out, err := hostCommand(c, "schema", "diff", "--from", file, "--to", file, "--dev-url", realm.address)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Equals, "Schemas are synced, no changes to be made.\n")
}

// The former host self-comparison test used SQL type spellings as well as
// asynchronous and unique indexes. The YAML form keeps those spellings so
// this test covers both type conversion and the prepared index kinds.
func TestHostDiffsYDBTypesAndIndexesAgainstThemselves_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	const schema = "tables:\n  accounts:\n    columns:\n" +
		"      id: {type: BIGINT, primary: true}\n" +
		"      email: {type: \"VARCHAR(255)\"}\n" +
		"      balance: {type: \"DECIMAL(22,9)\"}\n" +
		"      nickname: {type: TEXT}\n" +
		"    indexes:\n" +
		"      by_balance: {fields: [balance], type: async, include: [nickname]}\n" +
		"      accounts_email_key: {fields: [email], unique: true}\n"
	file := filepath.Join(writeFiles(c, c.TempDir(), map[string]string{"schema.yaml": schema}), "schema.yaml")

	out, err := hostCommand(c, "schema", "diff", "--from", file, "--to", file, "--dev-url", realm.address)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Equals, "Schemas are synced, no changes to be made.\n")
}

// A changed index kind still produces a replacement after both documents
// have gone through the provider's index preparation.
func TestHostDiffsChangedYDBIndexKind_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	const from = "CREATE TABLE `accounts` (id Int64 NOT NULL, login Utf8, PRIMARY KEY (id), INDEX by_login GLOBAL ON (login));\n"
	const to = "CREATE TABLE `accounts` (id Int64 NOT NULL, login Utf8, PRIMARY KEY (id), INDEX by_login GLOBAL ASYNC ON (login));\n"
	files := writeFiles(c, c.TempDir(), map[string]string{"from.sql": from, "to.sql": to})

	out, err := hostCommand(c, "schema", "diff", "--from", filepath.Join(files, "from.sql"), "--to", filepath.Join(files, "to.sql"), "--dev-url", realm.address)

	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "DROP INDEX")
	c.Assert(out, qt.Contains, "ADD INDEX")
	c.Assert(out, qt.Contains, "GLOBAL ASYNC")
}

// The former host's TestIndexesWithDialect_YDBRebuildsAChangedIndex compared
// indexes by their fixed YDB properties. Through the host and this provider,
// a changed kind, uniqueness, key, or covered column replaces the index under
// its same name rather than trying an in-place alteration.
func TestHostDiffsChangedYDBIndexProperties_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	for _, test := range []struct {
		name, before, after, added string
	}{
		{name: "made asynchronous", before: "GLOBAL SYNC ON (a)", after: "GLOBAL ASYNC ON (a)", added: "GLOBAL ASYNC"},
		{name: "a key column added", before: "GLOBAL SYNC ON (a)", after: "GLOBAL SYNC ON (a, b)", added: "ON (`a`, `b`)"},
		{name: "a covered column added", before: "GLOBAL SYNC ON (a)", after: "GLOBAL SYNC ON (a) COVER (c)", added: "COVER (`c`)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			checkHostIndexReplacement(c, realm.address, test.before, test.after, test.added)
		})
	}
}

func TestHostDiffsYDBIndexToUnique_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireCapability(c, targetCapabilities(c), tablecreate.UniqueIndexOnExistingTableCapability)
	realm := newCLIRealm(c)
	checkHostIndexReplacement(c, realm.address, "GLOBAL SYNC ON (a)", "GLOBAL UNIQUE SYNC ON (a)", "GLOBAL UNIQUE")
}

func checkHostIndexReplacement(c *qt.C, devURL, before, after, added string) {
	c.Helper()
	const table = "CREATE TABLE `records` (id Int64 NOT NULL, a Utf8, b Utf8, c Utf8, " +
		"PRIMARY KEY (id), INDEX `by_a` %s);\n"
	files := writeFiles(c, c.TempDir(), map[string]string{
		"from.sql": strings.Replace(table, "%s", before, 1),
		"to.sql":   strings.Replace(table, "%s", after, 1),
	})
	out, err := hostCommand(c, "schema", "diff", "--from", filepath.Join(files, "from.sql"),
		"--to", filepath.Join(files, "to.sql"), "--dev-url", devURL)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(strings.Count(out, "DROP INDEX `by_a`"), qt.Equals, 1, qt.Commentf("%s", out))
	c.Assert(strings.Count(out, "ADD INDEX `by_a`"), qt.Equals, 1, qt.Commentf("%s", out))
	c.Assert(strings.Index(out, "DROP INDEX `by_a`") < strings.Index(out, "ADD INDEX `by_a`"), qt.IsTrue,
		qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, added)
}

// YDB has no hash index method. A declaration asking for one must refuse the
// diff rather than turn an existing index into an unsupported index.
func TestHostDiffsUnsupportedYDBIndexMethod_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	realm := newCLIRealm(c)
	const table = "tables:\n  records:\n    columns:\n" +
		"      id: {type: Int64, primary: true, not_null: true}\n" +
		"      a: {type: Utf8}\n    indexes:\n      by_a: {fields: [a]%s}\n"
	files := writeFiles(c, c.TempDir(), map[string]string{
		"from.yaml": strings.Replace(table, "%s", "", 1),
		"to.yaml":   strings.Replace(table, "%s", ", type: hash", 1),
	})
	out, err := hostCommand(c, "schema", "diff", "--from", filepath.Join(files, "from.yaml"),
		"--to", filepath.Join(files, "to.yaml"), "--dev-url", realm.address)
	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, `index method "hash" has no YDB counterpart`)
	c.Assert(out, qt.Not(qt.Contains), "ADD INDEX")
}

// A vector index whose key gains a prefix is replaced under its original
// name, as in the former host's changed-vector comparison test.
func TestHostDiffsChangedYDBVectorIndexPrefix_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexKindsFeature)
	realm := newCLIRealm(c)
	const table = "tables:\n  docs:\n    columns:\n" +
		"      id: {type: Uint64, primary: true, not_null: true}\n" +
		"      gen: {type: Uint64}\n      emb: {type: String}\n      body: {type: Utf8}\n" +
		"    indexes:\n      by_emb: %s\n"
	const vector = "{fields: [%s], type: vector_kmeans_tree, distance: cosine, " +
		"vector_type: float, vector_dimension: 3, levels: 1, clusters: 2}"
	files := writeFiles(c, c.TempDir(), map[string]string{
		"from.yaml": strings.Replace(table, "%s", strings.Replace(vector, "%s", "emb", 1), 1),
		"to.yaml":   strings.Replace(table, "%s", strings.Replace(vector, "%s", "gen, emb", 1), 1),
	})
	out, err := hostCommand(c, "schema", "diff", "--from", filepath.Join(files, "from.yaml"),
		"--to", filepath.Join(files, "to.yaml"), "--dev-url", realm.address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(strings.Count(out, "DROP INDEX `by_emb`"), qt.Equals, 1, qt.Commentf("%s", out))
	c.Assert(strings.Count(out, "ADD INDEX `by_emb`"), qt.Equals, 1, qt.Commentf("%s", out))
	c.Assert(strings.Index(out, "DROP INDEX `by_emb`") < strings.Index(out, "ADD INDEX `by_emb`"), qt.IsTrue,
		qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "ON (`gen`, `emb`)")
}
