//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// requireHostLacks skips a test of what a host that does not negotiate
// feature does, on a host that negotiates it.
func requireHostLacks(c *qt.C, feature string) {
	c.Helper()
	host, manifest := hostManifest(c)
	if hostNegotiates(c, host, manifest, feature) {
		c.Skipf("the selected host negotiates %s", feature)
	}
}

// listLosses has the host list each detail an HCL document leaves out, one
// line per detail with its path and message, where it otherwise reports only
// their count.
func listLosses(c *qt.C) {
	c.Helper()
	c.Setenv("PTAH_LOG_LEVEL", "debug")
}

// platformItems is the HCL document of directory's items table with a TTL in
// its platform "ydb" block, as the host's embedded engine reads and writes it.
func platformItems(directory, interval string) string {
	return `table "items" {
  schema = schema.` + directory + `
  platform "ydb" {
    override "row_deletion_column" {
      value = "ts"
    }
    override "row_deletion_interval" {
      value = "` + interval + `"
    }
  }
  column "id" {
    type = Uint64
  }
  column "ts" {
    type = Timestamp
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}

schema "` + directory + `" {
}
`
}

// withoutPlatformBlocks is an HCL document without its tables' platform "ydb"
// blocks.
func withoutPlatformBlocks(document string) string {
	return regexp.MustCompile(`(?s)  platform "ydb" \{.*?\n  \}\n`).ReplaceAllString(document, "")
}

// The host's TestYDBCompatBinary_InspectWritesTheTTLAsPlatformProperties, on
// the native surface. A row table's TTL in its platform "ydb" block creates
// the table with that TTL. `ptah schema inspect --format hcl` writes the TTL
// back as the same block, with the unit of an integer column, and warns only
// about a setting HCL has no spelling for, in the embedded engine's words. The
// document applied back plans nothing, and so does the same document without
// the blocks: a table that writes none keeps its TTL. A changed interval and a
// changed unit are each altered in place, and the next run is synced.
func TestHostHCLPlatformTTL_RowTables(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.HCLPlatformFeature)
	listLosses(c)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))

	created, err := hostApplySource(c, directory, "hcl", platformItems(directory, "P30D"))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(created, qt.Contains, ") WITH (TTL = Interval(\"P30D\") ON `ts`);\n", qt.Commentf("%s", created))
	c.Assert(heldStorage(c, opened, path.Join(database, directory, "items")).TTL, qt.Equals, "ts 2592000s")
	events := path.Join(database, directory, "events")
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+events+"` (id Uint64 NOT NULL, at Uint64, PRIMARY KEY (id)) "+
		"WITH (TTL = Interval(\"PT1H\") ON at AS SECONDS, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3)"), qt.IsNil)
	document, notes := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")
	c.Assert(document, qt.Contains, `table "events" {
  schema = schema.`+directory+`
  platform "ydb" {
    override "row_deletion_column" {
      value = "at"
    }
    override "row_deletion_interval" {
      value = "PT1H"
    }
    override "row_deletion_unit" {
      value = "SECONDS"
    }
  }
`, qt.Commentf("%s", document))
	c.Assert(document, qt.Contains, platformItems(directory, "P30D")[:strings.Index(platformItems(directory, "P30D"), `  column "id"`)], qt.Commentf("%s", document))
	c.Assert(notes, qt.Contains, "table."+directory+".events: YDB table settings (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3) are not represented in HCL")
	c.Assert(notes, qt.Not(qt.Contains), "row-table-properties is not represented in HCL")
	c.Assert(notes, qt.Not(qt.Contains), "table."+directory+".items: YDB")
	for _, written := range []string{document, withoutPlatformBlocks(document)} {
		replanned, err := hostReplanSource(c, directory, "hcl", written)
		c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", written, replanned))
		c.Assert(replanned, qt.Contains, "Schema is synced, no changes to be made", qt.Commentf("%s\n%s", written, replanned))
	}
	changed := strings.NewReplacer(`value = "P30D"`, `value = "P7D"`, `value = "SECONDS"`, `value = "MILLISECONDS"`).Replace(document)
	altered, err := hostApplySource(c, directory, "hcl", changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", altered))
	c.Assert(heldStorage(c, opened, path.Join(database, directory, "items")).TTL, qt.Equals, "ts 604800s")
	c.Assert(heldStorage(c, opened, events).TTL, qt.Equals, "at 3600s MILLISECONDS")
	replanned, err := hostReplanSource(c, directory, "hcl", changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", replanned))
	c.Assert(replanned, qt.Contains, "Schema is synced, no changes to be made", qt.Commentf("%s", replanned))
}

// A column table's TTL that only deletes is written as the table's platform
// "ydb" block too, with the embedded engine's warning about the column
// storage HCL has no spelling for. The document applied back plans nothing:
// it cannot say the table is a column table, and the table keeps its storage.
// A changed interval is the column table's TTL change, and the next run is
// synced.
func TestHostHCLPlatformTTL_ColumnTable(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.HCLPlatformFeature)
	listLosses(c)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+path.Join(database, directory, "events")+"` "+
		"(ts Timestamp NOT NULL, id Uint64 NOT NULL, PRIMARY KEY (ts, id)) PARTITION BY HASH(id) WITH (STORE = COLUMN, TTL = Interval(\"P1D\") ON ts)"), qt.IsNil)

	document, notes := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")

	c.Assert(document, qt.Contains, `  platform "ydb" {
    override "row_deletion_column" {
      value = "ts"
    }
    override "row_deletion_interval" {
      value = "P1D"
    }
  }
`, qt.Commentf("%s", document))
	c.Assert(notes, qt.Contains, "table."+directory+".events: YDB column storage, hash partitioning and tiered TTL are not represented in HCL")
	for _, written := range []string{document, withoutPlatformBlocks(document)} {
		replanned, err := hostReplanSource(c, directory, "hcl", written)
		c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", written, replanned))
		c.Assert(replanned, qt.Contains, "Schema is synced, no changes to be made", qt.Commentf("%s\n%s", written, replanned))
	}
	changed := strings.Replace(document, `value = "P1D"`, `value = "P2D"`, 1)
	altered, err := hostApplySource(c, directory, "hcl", changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", altered))
	c.Assert(altered, qt.Contains, "ALTER TABLE `"+directory+"/events` SET (TTL = Interval(\"PT172800S\") DELETE ON `ts`);\n", qt.Commentf("%s", altered))
	again, _ := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")
	c.Assert(again, qt.Contains, `value = "P2D"`, qt.Commentf("%s", again))
	replanned, err := hostReplanSource(c, directory, "hcl", changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", replanned))
	c.Assert(replanned, qt.Contains, "Schema is synced, no changes to be made", qt.Commentf("%s", replanned))
}

// An override the provider's claim does not list is refused by the host at
// its block, naming the overrides the group takes, before anything reaches
// the database.
func TestHostHCLPlatformTTL_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.HCLPlatformFeature)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	misspelled := strings.Replace(platformItems(directory, "P30D"), `override "row_deletion_column"`, `override "row_deletion_colum"`, 1)

	output, err := hostApplySource(c, directory, "hcl", misspelled)

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `unknown table platform "ydb" override "row_deletion_colum": the group takes row_deletion_column, row_deletion_interval, row_deletion_unit`)
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	c.Assert(listing.Children, qt.HasLen, 0)
}

// A host that reads a table's platform "ydb" block exports a Go source as HCL
// through this provider with the table's TTL in the block, and warns about the
// table setting beside it that HCL has no spelling for, in the embedded
// engine's words. The document applied back to the database the Go source
// made plans nothing.
func TestHostExportsAGoSourceWithATTLAsHCL(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.HCLPlatformFeature)
	opened, directory, database := lockFixture(c)
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(database, directory, "workers"))
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "events")+"`"), qt.IsNil)
	})
	goSource := strings.ReplaceAll(exportedGo, "{dir}", directory)
	output, err := hostApplySource(c, directory, "go", goSource)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	document, notes := hostExportHCL(c, goSource)

	c.Assert(document, qt.Contains, "  platform \"ydb\" {\n    override \"row_deletion_column\" {\n      value = \"created_at\"\n    }\n"+
		"    override \"row_deletion_interval\" {\n      value = \"P1D\"\n    }\n  }\n", qt.Commentf("%s", document))
	c.Assert(notes, qt.Contains, "warning: table."+directory+".events: YDB table settings (AUTO_PARTITIONING_BY_LOAD = ENABLED) are not represented in HCL", qt.Commentf("%s", notes))
	output, err = hostApplySource(c, directory, "hcl", document)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", document, output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", document, output))
}

// Ported from the host's TestYDBCompatBinary_InspectWritesTheTTLAsPlatformProperties.
// `ptah-compat schema inspect` writes a table's TTL into HCL as its platform
// "ydb" block, in the spelling YDB shows, and reports no loss of it. The
// document applied back to the database it came from plans nothing, and so
// does the same document without the block: HCL that names no TTL leaves the
// table's TTL alone rather than losing it to a RESET (TTL).
func TestHostCompatInspectWritesTheTTLAsPlatformProperties(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	requireHostNegotiates(c, provider.HCLPlatformFeature)
	listLosses(c)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "events")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` (`id` Int64 NOT NULL, `created_at` Timestamp, PRIMARY KEY (`id`)) "+
		"WITH (TTL = Interval('P1D') ON `created_at`)"), qt.IsNil)

	document, notes, reapplied := compatInspect(c, directory)

	c.Assert(document, qt.Contains, `  platform "ydb" {
    override "row_deletion_column" {
      value = "created_at"
    }
    override "row_deletion_interval" {
      value = "P1D"
    }
  }
`, qt.Commentf("%s", document))
	c.Assert(notes, qt.Not(qt.Contains), "row-table-properties is not represented in HCL")
	c.Assert(reapplied, qt.Equals, "Schema is synced, no changes to be made\n")
	desired := "file://" + compatFile(c, c.TempDir(), "without-ttl.hcl", withoutPlatformBlocks(document))
	without, applyNotes, exit := runCompat(c, nil, "schema", "apply", "--url", os.Getenv("PTAH_YDB_TEST_URL"), "--schema", directory, "--to", desired, "--dry-run")
	c.Assert(exit, qt.Equals, 0, qt.Commentf("%s\n%s", without, applyNotes))
	c.Assert(without, qt.Equals, "Schema is synced, no changes to be made\n")
	c.Assert(heldStorage(c, opened, absolute).TTL, qt.Equals, "created_at 86400s")
}

// The TTL row of TestHostCompatInspectedHCLKeepsWhatItCannotWrite_HappyPath,
// on a host that does not negotiate source.hcl-platform. HCL then has no
// spelling for a table's TTL through this provider: `ptah-compat schema
// inspect` reports the row-table facet as not in the document, and the
// document applied back plans nothing and keeps the TTL.
func TestHostCompatInspectedHCLKeepsATTLItCannotWrite(t *testing.T) {
	c := testcheck.New(t)
	hostCompat(c)
	requireHostLacks(c, provider.HCLPlatformFeature)
	listLosses(c)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "events")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+absolute+"` (`id` Int64 NOT NULL, `created_at` Timestamp, PRIMARY KEY (`id`)) "+
		"WITH (TTL = Interval('P1D') ON `created_at`)"), qt.IsNil)

	_, notes, reapplied := compatInspect(c, directory)

	c.Assert(notes, qt.Contains, "table."+directory+".events: feature facet "+schema.TableProperties+" is not represented in HCL")
	c.Assert(reapplied, qt.Equals, "Schema is synced, no changes to be made\n")
	c.Assert(heldStorage(c, opened, absolute).TTL, qt.Equals, "created_at 86400s")
}

// A read reports an empty changefeed set and an empty native index set for
// every table that holds neither, and an HCL export lists neither as a detail
// it leaves out, as the host's embedded engine lists none: the table's
// platform part writes them by writing nothing. A changefeed the table holds
// is left out by name. The document applied back plans nothing, so the table
// keeps its changefeed.
func TestHostHCLExportListsNoEmptySetAsALoss_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.HCLPlatformFeature)
	listLosses(c)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	plain, feed := path.Join(database, directory, "plain"), path.Join(database, directory, "feed")
	for _, statement := range []string{
		"CREATE TABLE `" + plain + "` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id))",
		"CREATE TABLE `" + feed + "` (id Uint64 NOT NULL, PRIMARY KEY (id))",
		"ALTER TABLE `" + feed + "` ADD CHANGEFEED `updates` WITH (MODE = 'UPDATES', FORMAT = 'JSON')",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	document, notes := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")

	c.Assert(notes, qt.Not(qt.Contains), schema.ChangefeedProperties, qt.Commentf("%s", notes))
	c.Assert(notes, qt.Not(qt.Contains), schema.IndexProperties, qt.Commentf("%s", notes))
	c.Assert(notes, qt.Not(qt.Contains), "table."+directory+".plain:", qt.Commentf("%s", notes))
	c.Assert(notes, qt.Contains, "table."+directory+".feed: changefeed updates is not represented in HCL", qt.Commentf("%s", notes))
	replanned, err := hostReplanSource(c, directory, "hcl", document)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", document, replanned))
	c.Assert(replanned, qt.Contains, "Schema is synced, no changes to be made", qt.Commentf("%s\n%s", document, replanned))
	held, found := describedChangefeeds(c, opened, feed)
	c.Assert(found, qt.IsTrue)
	c.Assert(held, qt.HasLen, 1)
}

// A read reports a facet for every column, which for a column of the default
// family states nothing, and an HCL export lists no such facet as a detail it
// leaves out. A column of another family is left out by name, beside the
// family, and the document applied back plans nothing: the column stays in
// its family.
func TestHostHCLExportListsNoColumnOfTheDefaultFamilyAsALoss_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.HCLPlatformFeature)
	listLosses(c)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	plain, family := path.Join(database, directory, "plain"), path.Join(database, directory, "family")
	for _, statement := range []string{
		"CREATE TABLE `" + plain + "` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id))",
		"CREATE TABLE `" + family + "` (id Uint64 NOT NULL, body Utf8 FAMILY cold, PRIMARY KEY (id), FAMILY cold (COMPRESSION = \"lz4\"))",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}

	document, notes := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")

	c.Assert(notes, qt.Not(qt.Contains), schema.ColumnProperties, qt.Commentf("%s", notes))
	c.Assert(notes, qt.Not(qt.Contains), "Plain.id:", qt.Commentf("%s", notes))
	c.Assert(notes, qt.Contains, "table."+directory+".family: column family cold is not represented in HCL", qt.Commentf("%s", notes))
	c.Assert(notes, qt.Contains, ".body: its column family cold is not represented in HCL", qt.Commentf("%s", notes))
	replanned, err := hostReplanSource(c, directory, "hcl", document)
	c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", document, replanned))
	c.Assert(replanned, qt.Contains, "Schema is synced, no changes to be made", qt.Commentf("%s\n%s", document, replanned))
}
