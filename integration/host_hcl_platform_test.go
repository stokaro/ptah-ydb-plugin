//go:build integration

package integration_test

import (
	"context"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah/provider-sdk/provider"
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
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))

	created, err := hostApplySource(c, directory, "hcl", platformItems(directory, "P30D"))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(created, qt.Contains, "row-table-create-operation", qt.Commentf("%s", created))
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
	c.Assert(notes, qt.Contains, "warning: table."+directory+".events: YDB table settings (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3) are not represented in HCL")
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
	c.Assert(notes, qt.Contains, "warning: table."+directory+".events: YDB column storage, hash partitioning and tiered TTL are not represented in HCL")
	for _, written := range []string{document, withoutPlatformBlocks(document)} {
		replanned, err := hostReplanSource(c, directory, "hcl", written)
		c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", written, replanned))
		c.Assert(replanned, qt.Contains, "Schema is synced, no changes to be made", qt.Commentf("%s\n%s", written, replanned))
	}
	changed := strings.Replace(document, `value = "P1D"`, `value = "P2D"`, 1)
	altered, err := hostApplySource(c, directory, "hcl", changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", altered))
	c.Assert(altered, qt.Contains, "column-table-ttl-operation", qt.Commentf("%s", altered))
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
