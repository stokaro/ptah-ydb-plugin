//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// tieredFixture is a fixture directory on a server with tiering and external
// data sources on, the two secret objects a data source there reads its AWS
// keys from, and a provider session that reads the column table records back.
// The directory, the secrets and the flags are restored when the test ends.
type tieredFixture struct {
	directory, database, access, secret string
	read                                func(*qt.C) string
	source                              func(*qt.C) string
}

func newTieredFixture(c *qt.C) tieredFixture {
	c.Helper()
	monitoredAddress(c)
	opened, directory, database := lockFixture(c)
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableTieringInColumnShard": true, "EnableExternalDataSources": true})
	suffix := strings.ToLower(rand.Text())[:12]
	fixture := tieredFixture{directory: directory, database: database, access: "ptah_tiered_access_" + suffix, secret: "ptah_tiered_secret_" + suffix}
	for _, object := range []string{fixture.access, fixture.secret} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE OBJECT "+object+" (TYPE SECRET) WITH (value='fixture')"), qt.IsNil)
	}
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// The table goes first: YDB keeps a data source while a TTL names it.
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, "records")+"`"), qt.IsNil)
		emptyTree(ctx, opened, path.Join(database, directory))
		for _, object := range []string{fixture.access, fixture.secret} {
			c.Check(execFixtureSchema(ctx, opened.DB, "DROP OBJECT "+object+" (TYPE SECRET)"), qt.IsNil)
		}
	})
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	fixture.read = func(c *qt.C) string {
		c.Helper()
		for _, record := range observedTable(c, client, session, directory).Features.Facets.Records {
			if record.Kind == schema.ColumnStorageProperties {
				var storage native.ColumnStorageObservation
				c.Assert(protojson.Unmarshal(record.Value.Value.Payload, &storage), qt.IsNil)
				return heldTiers(storage.GetTtl())
			}
		}
		c.Fatalf("records was not read as a column table")
		return ""
	}
	fixture.source = func(c *qt.C) string {
		c.Helper()
		return describeSource(c, opened, path.Join(database, directory, "archive")).Location
	}
	return fixture
}

// heldTiers writes the tiers of a column table's TTL as the seconds after
// which each applies and what it does: the data source it moves rows to, or
// delete.
func heldTiers(ttl *native.ColumnTTLPolicy) string {
	var tiers []string
	for _, tier := range ttl.GetEnabled().GetTiers() {
		action := "delete"
		if source := tier.GetExternalStorage(); source != "" {
			action = source
		}
		tiers = append(tiers, fmt.Sprintf("%d %s", tier.GetApplyAfterSeconds(), action))
	}
	return strings.Join(tiers, ", ")
}

// document declares, in the fixture directory, the data source archive at
// location and records, a column table keyed by (ts, id) hashed by id in one
// shard. ttl is the further YAML of records: the TTL of its column_store key,
// or its platform group.
func (f tieredFixture) document(location, ttl string) string {
	return f.documentWithout(false, location, ttl)
}

// documentWithout is document, without the data source when dropped is true.
func (f tieredFixture) documentWithout(dropped bool, location, ttl string) string {
	source := "external_data_sources:\n  archive:\n    schema: " + f.directory + "\n    source_type: ObjectStorage\n    location: " + location + "\n" +
		"    auth_method: AWS\n    options:\n      AWS_REGION: us-east-1\n      AWS_ACCESS_KEY_ID_SECRET_NAME: " + f.access + "\n" +
		"      AWS_SECRET_ACCESS_KEY_SECRET_NAME: " + f.secret + "\n"
	if dropped {
		source = ""
	}
	return source + "tables:\n  records:\n    schema: " + f.directory + "\n" + strings.ReplaceAll(ttl, "{archive}", f.archive()) +
		"    columns:\n      ts: {type: Timestamp, primary: true, not_null: true}\n      id: {type: Uint64, primary: true, not_null: true}\n      body: {type: Utf8}\n"
}

// archive is the absolute path of the data source the documents declare.
func (f tieredFixture) archive() string { return path.Join(f.database, f.directory, "archive") }

const (
	// tieredTTL moves the rows of records to archive a day after ts and
	// deletes them a week after.
	tieredTTL = "    column_store:\n      hash_columns: [id]\n      partitions: 1\n" +
		"      ttl: {column: ts, tiers: [{interval: P1D, external_source: \"{archive}\"}, {interval: P7D}]}\n"
	// deletingTTL deletes the rows of records a week after ts.
	deletingTTL = "    column_store:\n      hash_columns: [id]\n      partitions: 1\n" +
		"    platform:\n      ydb: {row_deletion_column: ts, row_deletion_interval: P7D}\n"
	// deletingSoonerTTL deletes them two days after ts.
	deletingSoonerTTL = "    column_store:\n      hash_columns: [id]\n      partitions: 1\n" +
		"    platform:\n      ydb: {row_deletion_column: ts, row_deletion_interval: P2D}\n"
)

// Ported from the host's TestYDBColumnStore_TieredTTL. The data source and the
// column table whose TTL moves rows to it share a plan, which creates the
// source before the TTL names it. Moved to another location, the source is
// dropped and created again, which YDB refuses while a TTL names it, so the
// table's TTL is reset before and set again after, as the host's embedded
// engine carries it. A TTL that only deletes takes the tiered one's place by a
// RESET and a SET, and another one that only deletes by the SET alone. After
// each, the server holds what the document declares and nothing is left to
// plan.
func TestHostCarriesATieredTTLPastItsSourceReplacement_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	fixture := newTieredFixture(c)
	const first, replaced = "https://column.invalid/archive/", "https://column.invalid/replaced/"
	subject := fixture.directory + ".records"
	for _, run := range []struct {
		name, location, ttl string
		units               []string
		tiers               string
	}{
		{"the creation", first, tieredTTL, []string{"external-data-source-operation " + fixture.directory + ".archive", "column-table-create-operation " + subject},
			"86400 " + fixture.archive() + ", 604800 delete"},
		{"the source moved", replaced, tieredTTL,
			[]string{"column-table-ttl-operation " + subject, "external-data-source-operation " + fixture.directory + ".archive", "column-table-ttl-operation " + subject},
			"86400 " + fixture.archive() + ", 604800 delete"},
		{"a TTL that only deletes", replaced, deletingTTL, []string{"column-table-ttl-operation " + subject, "column-table-ttl-operation " + subject}, "604800 delete"},
		{"another TTL that only deletes", replaced, deletingSoonerTTL, []string{"column-table-ttl-operation " + subject}, "172800 delete"},
	} {
		output, err := hostApplySelected(c, fixture.directory, "yaml", fixture.document(run.location, run.ttl))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(pluginUnits(output), qt.DeepEquals, run.units, qt.Commentf("%s: %s", run.name, output))
		synced, err := hostApplySelected(c, fixture.directory, "yaml", fixture.document(run.location, run.ttl))
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, synced))
		c.Assert(synced, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s", run.name))
		c.Assert(fixture.read(c), qt.Equals, run.tiers, qt.Commentf("%s", run.name))
		c.Assert(fixture.source(c), qt.Equals, run.location, qt.Commentf("%s", run.name))
	}
}

// A column table and the data source its TTL moves rows to, both removed from
// the document, are dropped table first, as the host's embedded engine drops
// them: YDB refuses to drop a data source a table's TTL names.
func TestHostDropsATieredTableBeforeItsSource_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	fixture := newTieredFixture(c)
	applyUntilSynced(c, fixture.directory, "yaml", fixture.document("https://column.invalid/archive/", tieredTTL))

	output, err := hostApplySelected(c, fixture.directory, "yaml", "tables: {}\n")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(pluginUnits(output), qt.DeepEquals, []string{"table-drop-operation " + fixture.directory + ".records",
		"external-data-source-operation " + fixture.directory + ".archive"}, qt.Commentf("%s", output))
	synced, err := hostApplySelected(c, fixture.directory, "yaml", "tables: {}\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", synced))
	c.Assert(synced, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
}

// A document that keeps a column table whose TTL moves rows to a data source
// and drops the source is refused before anything is written, in the host's
// embedded engine's words, and the source stays.
func TestHostRefusesDroppingASourceATieredTTLNames_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	fixture := newTieredFixture(c)
	const location = "https://column.invalid/archive/"
	applyUntilSynced(c, fixture.directory, "yaml", fixture.document(location, tieredTTL))

	output, err := hostApplySelected(c, fixture.directory, "yaml", fixture.documentWithout(true, location, tieredTTL))

	c.Assert(err, qt.IsNotNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "table "+fixture.directory+"/records: its TTL reads external data source "+fixture.directory+"/archive, which the plan drops")
	c.Assert(fixture.source(c), qt.Equals, location)
	c.Assert(fixture.read(c), qt.Equals, "86400 "+fixture.archive()+", 604800 delete")
}
