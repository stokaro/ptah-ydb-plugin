//go:build integration

package integration_test

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// builtInSpellingTables are the tables whose exports the host's embedded
// engine and this provider must write alike: one with YDB's settings for a
// new table, one with settings of its own, and one with a synchronous and an
// asynchronous global index. {dir} is the table's directory.
var builtInSpellingTables = []string{
	"CREATE TABLE `{dir}/plain` (id Uint64 NOT NULL, n Int32, PRIMARY KEY (id))",
	"CREATE TABLE `{dir}/s` (id Uint64 NOT NULL, PRIMARY KEY (id)) WITH (AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 4)",
	"CREATE TABLE `{dir}/t` (id Uint64 NOT NULL, a Utf8, b Utf8, PRIMARY KEY (id), INDEX i_sync GLOBAL SYNC ON (a), INDEX i_async GLOBAL ASYNC ON (b))",
}

// builtInSpellingFixture creates builtInSpellingTables in a directory of the
// fixture's and returns it.
func builtInSpellingFixture(c *qt.C) string {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	for _, statement := range builtInSpellingTables {
		statement = strings.ReplaceAll(statement, "{dir}", path.Join(database, directory))
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	return directory
}

// builtInSpellingHCL is the HCL `ptah-compat schema inspect` writes for
// builtInSpellingTables in directory, below its feature coverage header, as
// the host's embedded engine at 74de0b65b wrote it on 25.1.4.7 and 26.2.1.14:
// no setting YDB gives every new table, and each global index with its type.
func builtInSpellingHCL(directory string) string {
	return strings.ReplaceAll(`schema "DIR" {
}

table "plain" {
  schema = schema.DIR
  column "id" {
    type = Uint64
  }
  column "n" {
    type = Int32
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}

table "s" {
  schema = schema.DIR
  column "id" {
    type = Uint64
  }
  primary_key {
    columns = [column.id]
  }
}

table "t" {
  schema = schema.DIR
  column "id" {
    type = Uint64
  }
  column "a" {
    type = Utf8
    null = true
  }
  column "b" {
    type = Utf8
    null = true
  }
  primary_key {
    columns = [column.id]
  }
  index "i_async" {
    type = "GLOBAL ASYNC"
    columns = [column.b]
  }
  index "i_sync" {
    type = "GLOBAL SYNC"
    columns = [column.a]
  }
}
`, "DIR", directory)
}

// builtInSpellingSQL is the CREATE TABLE statements of the YQL
// `ptah-compat schema inspect --format '{{ sql . }}'` writes for
// builtInSpellingTables in directory, as the host's embedded engine at
// 74de0b65b wrote them on 25.1.4.7 and 26.2.1.14: a WITH clause only for the
// settings YDB does not give every new table.
func builtInSpellingSQL(directory string) string {
	// A raw string cannot hold a backquote, so ~ stands for one.
	statements := `CREATE TABLE ~DIR/plain~ (
    ~id~ Uint64 NOT NULL,
    ~n~ Int32,
    PRIMARY KEY (~id~),
    FAMILY ~default~ (COMPRESSION = 'off')
);
CREATE TABLE ~DIR/s~ (
    ~id~ Uint64 NOT NULL,
    PRIMARY KEY (~id~),
    FAMILY ~default~ (COMPRESSION = 'off')
) WITH (AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 4);
CREATE TABLE ~DIR/t~ (
    ~id~ Uint64 NOT NULL,
    ~a~ Utf8,
    ~b~ Utf8,
    PRIMARY KEY (~id~),
    INDEX ~i_async~ GLOBAL ASYNC ON (~b~),
    INDEX ~i_sync~ GLOBAL SYNC ON (~a~),
    FAMILY ~default~ (COMPRESSION = 'off')
);
`
	return strings.ReplaceAll(strings.ReplaceAll(statements, "~", "`"), "DIR", directory)
}

// withoutCoverageHeader is an inspected HCL document without its feature
// coverage header, whose model names are the provider's own, and the empty
// line after it.
func withoutCoverageHeader(document string) string {
	if !strings.HasPrefix(document, "// ptah:feature-coverage ") {
		return document
	}
	_, rest, _ := strings.Cut(document, "\n")
	return strings.TrimPrefix(rest, "\n")
}

// createStatements is the CREATE TABLE statements of an inspected YQL
// document, each with its line end, in the document's order.
func createStatements(document string) string {
	var statements []string
	for statement := range strings.SplitAfterSeq(document, ";\n") {
		if strings.HasPrefix(statement, "CREATE TABLE ") {
			statements = append(statements, statement)
		}
	}
	return strings.Join(statements, "")
}

// `ptah-compat schema inspect` writes a table as the host's embedded engine
// wrote it, in HCL and in YQL: the settings YDB gives every new table are no
// part of a description, so a table that holds only them has no WITH clause,
// and a global index has the type the embedded engine read it as, GLOBAL
// SYNC or GLOBAL ASYNC.
func TestHostCompatInspectWritesTheBuiltInSpellings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	directory := builtInSpellingFixture(c)

	document, notes, exit := runCompat(c, nil, "schema", "inspect", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", directory)
	statements, sqlNotes, sqlExit := runCompat(c, nil, "schema", "inspect", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", directory,
		"--format", "{{ sql . }}")

	c.Assert(exit, qt.Equals, 0, qt.Commentf("schema inspect:\n%s", notes))
	c.Assert(withoutCoverageHeader(document), qt.Equals, builtInSpellingHCL(directory))
	c.Assert(sqlExit, qt.Equals, 0, qt.Commentf("schema inspect --format sql:\n%s", sqlNotes))
	c.Assert(createStatements(statements), qt.Equals, builtInSpellingSQL(directory))
}

// builtInSpellingDirectives are the table and index directives `ptah
// introspect` writes for builtInSpellingTables in directory, by file, as the
// host's embedded engine at 74de0b65b wrote them on 25.1.4.7: the settings
// YDB gives every new table left out, and each global index with its type.
// The embedded engine introspects only a server whose resource pools it can
// read, which the measurement turned on.
func builtInSpellingDirectives(directory string) map[string][]string {
	return map[string][]string{
		directory + "_plain.go": {`//ptah:schema:table name="plain" schema="` + directory + `"`},
		directory + "_s.go": {`//ptah:schema:table name="s" schema="` + directory +
			`" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="4"`},
		directory + "_t.go": {`//ptah:schema:table name="t" schema="` + directory + `"`,
			`//ptah:schema:index name="i_async" fields="b" type="GLOBAL ASYNC" table="` + directory + `.t"`,
			`//ptah:schema:index name="i_sync" fields="a" type="GLOBAL SYNC" table="` + directory + `.t"`},
	}
}

// tableDirectives reads the table and index directives of each file written
// for a table into dir, by file name.
func tableDirectives(c *qt.C, dir string, names []string) map[string][]string {
	c.Helper()
	result := make(map[string][]string)
	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(dir, name))
		c.Assert(err, qt.IsNil)
		for line := range strings.SplitSeq(string(body), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "//ptah:schema:table ") || strings.HasPrefix(line, "//ptah:schema:index ") {
				result[name] = append(result[name], line)
			}
		}
	}
	return result
}

// `ptah introspect` writes a table's directives as the host's embedded
// engine wrote them: no attribute for a setting YDB gives every new table,
// and each global index with the type the embedded engine read it as.
func TestHostIntrospectWritesTheBuiltInTableDirectives_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	directory := builtInSpellingFixture(c)
	out := c.TempDir()

	written, err := hostCommand(c, "introspect", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", directory, "--out", out)

	c.Assert(err, qt.IsNil, qt.Commentf("introspect:\n%s", written))
	want := builtInSpellingDirectives(directory)
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	c.Assert(tableDirectives(c, out, names), qt.DeepEquals, want)
}
