//go:build integration

package integration_test

import (
	"os"
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// hclTypeMoments is the type of the users table's moment column, by whether
// the line has the 64-bit date and time types.
var hclTypeMoments = map[bool]string{true: "Timestamp64", false: "Timestamp"}

// hclTypeStatements create, in the directory at absolute, a table with a
// BigSerial key, defaults of three kinds, a unique index and an asynchronous
// one with a covered column, and a table with a parameterized Decimal and a
// JsonDocument. moment is the type of the moment column.
func hclTypeStatements(absolute, moment string) []string {
	return []string{
		"CREATE TABLE `" + absolute + "/users` (id BigSerial NOT NULL, email Utf8 NOT NULL, nick Utf8 DEFAULT \"it's\", " +
			"created_at " + moment + " DEFAULT " + moment + "(\"2026-01-01T00:00:00Z\"), score Int32 DEFAULT 0, PRIMARY KEY (id), " +
			"INDEX users_email_uq GLOBAL UNIQUE SYNC ON (email), INDEX users_score_ix GLOBAL ASYNC ON (score) COVER (nick))",
		"CREATE TABLE `" + absolute + "/orders` (id Uint64 NOT NULL, total Decimal(22,9) NOT NULL, body JsonDocument, PRIMARY KEY (id))",
	}
}

// hclTypesDocument is the HCL `ptah-compat schema inspect` writes for the
// tables of hclTypeStatements in directory, below its feature coverage header.
func hclTypesDocument(directory, moment string) string {
	return strings.NewReplacer("DIR", directory, "MOMENT", moment).Replace(`schema "DIR" {
}

table "orders" {
  schema = schema.DIR
  column "id" {
    type = Uint64
  }
  column "total" {
    type = Decimal(22,9)
  }
  column "body" {
    type = JsonDocument
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}

table "users" {
  schema = schema.DIR
  column "id" {
    type = Int64
    auto_increment = true
  }
  column "email" {
    type = Utf8
  }
  column "nick" {
    type = Utf8
    null = true
    default = "it's"
  }
  column "created_at" {
    type = MOMENT
    null = true
    default = "2026-01-01T00:00:00Z"
  }
  column "score" {
    type = Int32
    null = true
    default = "0"
  }
  primary_key {
    columns = [column.id]
  }
  index "users_email_uq" {
    unique = true
    type = "GLOBAL SYNC"
    columns = [column.email]
  }
  index "users_score_ix" {
    type = "GLOBAL ASYNC"
    include = [column.nick]
    columns = [column.score]
  }
}
`)
}

// Ported from the host's TestRenderInspectedForAtlasCLIWritesYDBTypesAsYDBSpellsThem
// and TestRenderInspectedForAtlasCLIYDBDocumentReadsBackAsItself. Atlas has no
// HCL for YDB, so the document `ptah-compat schema inspect` writes is a Ptah
// extension, and this pins its shape: a type as YDB spells it, written bare,
// with the call form of a parameterized Decimal; a BigSerial column as Int64
// with auto_increment; each default as a string; an index's YDB kind as its
// type, with unique and include; a directory as a schema block; and tables in
// name order. The document reads back as itself: applied to the directory it
// came from it plans nothing, and applied to an empty directory it creates
// tables whose inspection writes the same document, so it cannot describe one
// schema and apply another. The host test also wrote a table at the database
// root with no schema reference; a scoped read never reaches the root, so this
// test does not either.
func TestHostCompatInspectedHCLSpellsYDBTypes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	moment := hclTypeMoments[targetCapabilities(c)["wide_date_time_types"]]
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	for _, statement := range hclTypeStatements(path.Join(database, directory), moment) {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	_, copied, _ := inspectionFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, copied))

	document, _, reapplied := compatInspect(c, directory)
	copyDocument := strings.ReplaceAll(withoutCoverageHeader(document), directory, copied)
	created, notes, exit := runCompat(c, nil, "schema", "apply", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", copied,
		"--to", "file://"+compatFile(c, c.TempDir(), "copy.hcl", copyDocument), "--auto-approve")
	copiedDocument, _, _ := compatInspect(c, copied)

	c.Assert(withoutCoverageHeader(document), qt.Equals, hclTypesDocument(directory, moment))
	c.Assert(reapplied, qt.Equals, "Schema is synced, no changes to be made\n")
	c.Assert(exit, qt.Equals, 0, qt.Commentf("%s\n%s", created, notes))
	c.Assert(withoutCoverageHeader(copiedDocument), qt.Equals, hclTypesDocument(copied, moment))
}
