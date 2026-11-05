//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// countriesTable is the reference table of the host's data tests. Its columns
// cover the YDB types a declared row is written into on every certified line:
// a declared TIMESTAMP and DATE land on the 64-bit types where the line has
// them, and DECIMAL(22,9) is the decimal 25.1 takes.
const countriesTable = "//ptah:schema:table name=\"countries\"\ntype Country struct {\n" +
	"\t//ptah:schema:field name=\"code\" type=\"VARCHAR(8)\" primary=\"true\"\n\tCode string\n" +
	"\t//ptah:schema:field name=\"rank\" type=\"INTEGER\"\n\tRank *int32\n" +
	"\t//ptah:schema:field name=\"population\" type=\"BIGINT\"\n\tPopulation *int64\n" +
	"\t//ptah:schema:field name=\"rate\" type=\"DECIMAL(22,9)\"\n\tRate *string\n" +
	"\t//ptah:schema:field name=\"joined\" type=\"TIMESTAMP\"\n\tJoined *string\n" +
	"\t//ptah:schema:field name=\"founded\" type=\"DATE\"\n\tFounded *string\n" +
	"\t//ptah:schema:field name=\"active\" type=\"BOOLEAN\"\n\tActive *bool\n" +
	"\t//ptah:schema:field name=\"ratio\" type=\"DOUBLE PRECISION\"\n\tRatio *float64\n" +
	"\t//ptah:schema:field name=\"flag\" type=\"BYTEA\"\n\tFlag []byte\n" +
	"\t//ptah:schema:field name=\"meta\" type=\"JSONB\"\n\tMeta *string\n" +
	"\t//ptah:schema:field name=\"uid\" type=\"UUID\"\n\tUID *string\n" +
	"\t//ptah:schema:field name=\"tiny\" type=\"SMALLINT UNSIGNED\"\n\tTiny *uint16\n}\n"

// countriesData declares the rows of countries.yaml for the table.
const countriesData = "//ptah:schema:data table=\"countries\" key=\"code\" file=\"countries.yaml\"\n"

// countriesRows are the host's declared row set in YAML: an integer for every
// integer column, the text a person writes for a decimal, a moment, a day, a
// UUID and a JSON document, and a float for a double. YAML has no spelling
// for bytes that are not text, so the BYTEA value is text here.
const countriesRows = "- {code: CZ, rank: 1, population: 10900000, rate: \"1.50\", joined: \"2004-05-01T00:00:00Z\", founded: \"1993-01-01\", " +
	"active: true, ratio: 0.5, flag: raw, meta: '{\"b\": 2, \"a\": 1.50}', uid: 550E8400-E29B-41D4-A716-446655440000, tiny: 7}\n" +
	"- {code: SK, rank: 2, population: 5400000, rate: null, joined: null, founded: \"1993-01-01\", active: false, ratio: null, " +
	"flag: null, meta: null, uid: null, tiny: null}\n"

// movedCountriesRows is countriesRows with the first rank changed and a third
// row.
const movedCountriesRows = "- {code: CZ, rank: 9, population: 10900000, rate: \"1.50\", joined: \"2004-05-01T00:00:00Z\", founded: \"1993-01-01\", " +
	"active: true, ratio: 0.5, flag: raw, meta: '{\"b\": 2, \"a\": 1.50}', uid: 550E8400-E29B-41D4-A716-446655440000, tiny: 7}\n" +
	"- {code: SK, rank: 2, population: 5400000, rate: null, joined: null, founded: \"1993-01-01\", active: false, ratio: null, " +
	"flag: null, meta: null, uid: null, tiny: null}\n" +
	"- {code: DE, rank: 3, population: 84000000, rate: 0.25, joined: \"1995-01-01T00:00:00Z\", founded: \"1990-10-03\", active: true, " +
	"ratio: 1.25, flag: null, meta: '{}', uid: null, tiny: 0}\n"

// declaredCountryRow is the host's declared row of the schema apply test, as
// a person writes it in YAML: the day of the moment unquoted, so YAML reads a
// timestamp, and no flag.
func declaredCountryRow(population string) string {
	return "- {code: CZ, rank: 1, population: " + population + ", rate: \"1.50\", joined: 2004-05-01, founded: \"1993-01-01\", active: true, " +
		"ratio: 0.5, meta: '{\"b\": 2, \"a\": 1}', uid: 550E8400-E29B-41D4-A716-446655440000, tiny: 7}\n"
}

// countriesFixture is a dev realm of its own that the pinned host works in
// through this provider, with the table declared alone and the table declared
// with its rows, and a migration directory.
type countriesFixture struct {
	opened                         *connection.Connection
	address, root                  string
	tableOnly, withRows, migration string
	host, manifest                 string
}

func newCountriesFixture(c *qt.C) *countriesFixture {
	c.Helper()
	host, manifest := hostManifest(c)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	opened, err := connection.Open(ctx, base)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	address, root := realmFixture(c, opened, base)
	// The table, the revision tables and the host's schema lock are all at
	// the realm's root.
	c.Cleanup(func() { emptyRealm(c, opened, root) })
	dir := c.TempDir()
	f := &countriesFixture{opened: opened, address: address, root: root, tableOnly: filepath.Join(dir, "table"), withRows: filepath.Join(dir, "rows"),
		migration: filepath.Join(dir, "migrations"), host: host, manifest: manifest}
	writeFiles(c, f.tableOnly, map[string]string{"entities.go": "package entities\n\n" + countriesTable})
	writeFiles(c, f.withRows, map[string]string{"entities.go": "package entities\n\n" + countriesData + countriesTable})
	return f
}

// declare writes the declared rows.
func (f *countriesFixture) declare(c *qt.C, rows string) {
	c.Helper()
	c.Assert(os.WriteFile(filepath.Join(f.withRows, "countries.yaml"), []byte(rows), 0o600), qt.IsNil)
}

// run runs one host command line against the realm and returns what the
// host printed and how it exited.
func (f *countriesFixture) run(c *qt.C, args ...string) (string, error) {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, f.host, append(args, "--provider-config", f.manifest, "--provider-engine", "ydb", "--db-url", f.address)...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

// apply runs `schema apply` over the Go sources of root.
func (f *countriesFixture) apply(c *qt.C, root string) (string, error) {
	c.Helper()
	return f.run(c, "schema", "apply", "--auto-approve", "--tx-mode", "none", "--root-dir", root)
}

// planned is the head of each unit `schema apply --dry-run --json` would run
// over the declared rows, in order, or none for a plan with nothing to run.
func (f *countriesFixture) planned(c *qt.C) []string {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, f.host, "schema", "apply", "--dry-run", "--json", "--tx-mode", "none", "--root-dir", f.withRows,
		"--provider-config", f.manifest, "--provider-engine", "ydb", "--db-url", f.address)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	c.Assert(cmd.Run(), qt.IsNil, qt.Commentf("%s\n%s", stdout.String(), stderr.String()))
	var report struct {
		Units []plannedUnit `json:"units"`
	}
	c.Assert(json.Unmarshal(stdout.Bytes(), &report), qt.IsNil, qt.Commentf("%s", stdout.String()))
	heads := make([]string, len(report.Units))
	for i, unit := range report.Units {
		heads[i] = unit.head()
	}
	return heads
}

// data runs `migrations data` over the declared rows into the migration
// directory, with the further flags given.
func (f *countriesFixture) data(c *qt.C, flags ...string) (string, error) {
	c.Helper()
	return f.run(c, append([]string{"migrations", "data", "--root-dir", f.withRows, "--migrations-dir", f.migration}, flags...)...)
}

// statements is the first word of each statement of a migration file, in
// order.
func (f *countriesFixture) statements(c *qt.C, name string) []string {
	c.Helper()
	body, err := os.ReadFile(filepath.Join(f.migration, name))
	c.Assert(err, qt.IsNil)
	var words []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if word, _, _ := strings.Cut(strings.TrimSpace(line), " "); word != "" && !strings.HasPrefix(word, "--") {
			words = append(words, word)
		}
	}
	return words
}

// stored reads the countries back, one line per row in key order, each value
// as YDB writes it as text, past the provider.
func (f *countriesFixture) stored(c *qt.C) []string {
	c.Helper()
	columns := []string{"rank", "population", "rate", "joined", "founded", "active", "ratio", "flag", "uid", "tiny"}
	casts := make([]string, len(columns))
	for i, column := range columns {
		casts[i] = "CAST(`" + column + "` AS Utf8)"
	}
	// #nosec G202 -- the columns are this test's own names, and the path is a table under a directory this test generated
	rows, err := f.opened.DB.QueryContext(c.Context(), "SELECT `code`, "+strings.Join(casts, ", ")+", `meta` FROM `"+path.Join(f.root, "countries")+"` ORDER BY `code`")
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(rows.Close(), qt.IsNil) }()
	var lines []string
	for rows.Next() {
		values := make([]sql.NullString, len(columns)+2)
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		c.Assert(rows.Scan(targets...), qt.IsNil)
		texts := make([]string, len(values))
		for i, value := range values {
			texts[i] = map[bool]string{true: value.String, false: "NULL"}[value.Valid]
		}
		lines = append(lines, strings.Join(texts, " | "))
	}
	c.Assert(rows.Err(), qt.IsNil)
	return lines
}

// writtenCountries is countriesRows as the table holds them: the decimal and
// the JSON document in their canonical forms and the UUID in lower case.
var writtenCountries = []string{
	"CZ | 1 | 10900000 | 1.5 | 2004-05-01T00:00:00Z | 1993-01-01 | true | 0.5 | raw | 550e8400-e29b-41d4-a716-446655440000 | 7 | {\"a\":1.5,\"b\":2}",
	"SK | 2 | 5400000 | NULL | NULL | 1993-01-01 | false | NULL | NULL | NULL | NULL | NULL",
}

// Ported from the host's TestYDBDataDiff_RoundTrips: `migrations data` writes
// a migration that inserts the declared rows into a table that holds none,
// and `migrations up` applies it through the provider; every value lands in
// its column's own type and reads back in its canonical form. A second
// `migrations data --dry-run` finds nothing to write, because the declared
// `1.50` pairs with the 1.5 the Decimal holds. The declaration then moves,
// `migrations data --allow-destructive` writes the update and the insert,
// `migrations up` applies them, nothing is left to write, and `migrations
// down` lands where the second migration started.
func TestHostDataMigrationsRoundTrip_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newCountriesFixture(c)
	created, err := f.apply(c, f.tableOnly)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	f.declare(c, countriesRows)

	first, firstErr := f.data(c)
	up, upErr := f.run(c, "migrations", "up", "--migrations-dir", f.migration)
	written := f.stored(c)
	converged, convergedErr := f.data(c, "--dry-run")
	f.declare(c, movedCountriesRows)
	moved, movedErr := f.data(c, "--allow-destructive")
	upMoved, upMovedErr := f.run(c, "migrations", "up", "--migrations-dir", f.migration)
	movedRows := f.stored(c)
	afterForward, afterForwardErr := f.data(c, "--dry-run")
	down, downErr := f.run(c, "migrations", "down", "--target", "1", "--confirm", "--migrations-dir", f.migration)
	afterBackward := f.stored(c)

	c.Assert(firstErr, qt.IsNil, qt.Commentf("%s", first))
	c.Assert(first, qt.Contains, "Wrote data migration version 1")
	c.Assert(f.statements(c, "0000000001_data.up.sql"), qt.DeepEquals, []string{"INSERT", "INSERT"})
	c.Assert(upErr, qt.IsNil, qt.Commentf("%s", up))
	c.Assert(written, qt.DeepEquals, writtenCountries)
	c.Assert(convergedErr, qt.IsNil, qt.Commentf("%s", converged))
	c.Assert(converged, qt.Equals, "no data changes\n")
	c.Assert(movedErr, qt.IsNil, qt.Commentf("%s", moved))
	c.Assert(moved, qt.Contains, "Wrote data migration version 2")
	c.Assert(f.statements(c, "0000000002_data.up.sql"), qt.DeepEquals, []string{"INSERT", "UPDATE"})
	c.Assert(upMovedErr, qt.IsNil, qt.Commentf("%s", upMoved))
	c.Assert(movedRows, qt.DeepEquals, []string{
		"CZ | 9 | 10900000 | 1.5 | 2004-05-01T00:00:00Z | 1993-01-01 | true | 0.5 | raw | 550e8400-e29b-41d4-a716-446655440000 | 7 | {\"a\":1.5,\"b\":2}",
		"DE | 3 | 84000000 | 0.25 | 1995-01-01T00:00:00Z | 1990-10-03 | true | 1.25 | NULL | NULL | 0 | {}",
		writtenCountries[1],
	})
	c.Assert(afterForwardErr, qt.IsNil, qt.Commentf("%s", afterForward))
	c.Assert(afterForward, qt.Equals, "no data changes\n")
	c.Assert(downErr, qt.IsNil, qt.Commentf("%s", down))
	c.Assert(afterBackward, qt.DeepEquals, writtenCountries)
}

// dayLiterals and momentLiterals are the literals the declared day and moment
// are written as, by whether the line has the 64-bit date and time types.
var (
	dayLiterals    = map[bool]string{true: "Date32('1993-01-01')", false: "Date('1993-01-01')"}
	momentLiterals = map[bool]string{true: "Timestamp64('2004-05-01T00:00:00Z')", false: "Timestamp('2004-05-01T00:00:00Z')"}
)

// Ported from the host's TestYDBDeclaredRows_ConvergeThroughSchemaApply: the
// host's `schema apply` creates the table and writes its declared row through
// the provider, and the next run finds nothing. A changed declared value
// plans exactly the one UPDATE that writes it, and after it the table is
// synced and holds the new value. The UPDATE is the host's text, the table
// named in the realm the run works in.
func TestHostConvergesDeclaredRowsThroughSchemaApply_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	f := newCountriesFixture(c)
	wide := targetCapabilities(c)["wide_date_time_types"]
	f.declare(c, declaredCountryRow("10900000"))

	first, firstErr := f.apply(c, f.withRows)
	again, againErr := f.apply(c, f.withRows)
	f.declare(c, declaredCountryRow("11000000"))
	changed := f.planned(c)
	applied, appliedErr := f.apply(c, f.withRows)
	afterChange, afterChangeErr := f.apply(c, f.withRows)

	c.Assert(firstErr, qt.IsNil, qt.Commentf("%s", first))
	c.Assert(first, qt.Matches, `(?s).*Schema apply completed successfully.*`)
	c.Assert(againErr, qt.IsNil, qt.Commentf("%s", again))
	c.Assert(again, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
	c.Assert(changed, qt.DeepEquals, []string{"UPDATE `countries` SET `active` = true, `founded` = " + dayLiterals[wide] + ", `joined` = " +
		momentLiterals[wide] + ", `meta` = JsonDocument('{\"a\":1,\"b\":2}'), `population` = 11000000l, `rank` = 1, " +
		"`rate` = Decimal('1.5', 22, 9), `ratio` = Double('0.5'), `tiny` = 7us, `uid` = Uuid('550e8400-e29b-41d4-a716-446655440000') WHERE `code` = 'CZ'u"})
	c.Assert(appliedErr, qt.IsNil, qt.Commentf("%s", applied))
	c.Assert(afterChangeErr, qt.IsNil, qt.Commentf("%s", afterChange))
	c.Assert(afterChange, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
	c.Assert(f.stored(c), qt.DeepEquals, []string{
		"CZ | 1 | 11000000 | 1.5 | 2004-05-01T00:00:00Z | 1993-01-01 | true | 0.5 | NULL | 550e8400-e29b-41d4-a716-446655440000 | 7 | {\"a\":1,\"b\":2}",
	})
}
