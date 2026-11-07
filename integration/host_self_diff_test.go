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
