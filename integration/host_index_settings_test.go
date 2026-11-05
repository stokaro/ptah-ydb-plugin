//go:build integration

package integration_test

import (
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/scheme"
)

// indexSettingsItems declares items in directory with a covering global index
// on body and an asynchronous one on n, each with the further attributes given
// on its index directive, the name of the index on n, and the further members
// given, such as a holder field of another index directive.
func indexSettingsItems(directory, bodySettings, nIndex, nSettings, members string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"" + directory + "\"\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" primary=\"true\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n" +
		"\t//ptah:schema:index name=\"by_body\" fields=\"body\" include=\"n\"" + bodySettings + "\n\tBody string\n" +
		"\t//ptah:schema:field name=\"n\" type=\"Int64\"\n" +
		"\t//ptah:schema:index name=\"" + nIndex + "\" fields=\"n\" type=\"async\"" + nSettings + "\n\tN int64\n" + members + "}\n"
}

// indexHolder is a holder field whose index directive declares index name on
// body and id with the further attributes given.
func indexHolder(name, attributes string) string {
	return "\t//ptah:schema:index name=\"" + name + "\" fields=\"body,id\"" + attributes + "\n\t_ int\n"
}

// heldSettings is what the server holds of the partitioning and read
// replicas of each index of the table at absolute, as the settings a source
// writes for them, by name: "min=2 load=ENABLED replicas=PER_AZ:1".
func heldSettings(c *qt.C, opened *connection.Connection, absolute string) map[string]string {
	c.Helper()
	session, err := nativeapi.OpenTable(c.Context(), opened.Driver)
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(session.Close(), qt.IsNil) }()
	described, err := session.Describe(c.Context(), absolute)
	c.Assert(err, qt.IsNil)
	result := map[string]string{}
	for _, index := range described.GetIndexes() {
		settings := tablecreate.HeldIndexSettings(index)
		partitioning := settings.GetPartitioningSettings()
		text := "min=" + strconv.FormatUint(partitioning.GetMinPartitionsCount(), 10) + " load=" + partitioning.GetPartitioningByLoad().String()
		if replicas := settings.GetReadReplicasSettings(); replicas.GetPerAzReadReplicasCount() != 0 {
			text += " replicas=PER_AZ:" + strconv.FormatUint(replicas.GetPerAzReadReplicasCount(), 10)
		}
		result[index.GetName()] = text
	}
	return result
}

// tableExists reports whether the database holds an entry at absolute.
func tableExists(c *qt.C, opened *connection.Connection, absolute string) bool {
	c.Helper()
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Dir(absolute))
	c.Assert(err, qt.IsNil)
	return slices.ContainsFunc(listing.Children, func(entry scheme.Entry) bool { return entry.Name == path.Base(absolute) })
}

// The host's YDB index partitioning tests through a Go source: the index
// directive states a global index's partitioning and read replicas, the
// creation gives each index its declared settings after the table, the next
// run is synced, and a changed setting is altered in place while a setting
// the source leaves out keeps what the index holds, as the host's embedded
// engine plans it.
func TestHostIndexSettingsFromGo_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	declared := indexSettingsItems(directory, ` auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="2"`, "by_n", ` read_replicas_settings="PER_AZ:1"`, "")
	for _, run := range []struct{ name, source, want string }{
		{"the creation", declared, `(?s).*row-table-create-operation.*row-table-index-settings-operation.*row-table-index-settings-operation.*Schema apply completed successfully.*`},
		{"a second run", declared, `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, run.source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(heldSettings(c, opened, absolute), qt.DeepEquals, map[string]string{
		"by_body": "min=2 load=ENABLED", "by_n": "min=1 load=DISABLED replicas=PER_AZ:1"})

	changed := indexSettingsItems(directory, ` auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="4"`, "by_n", "", "")
	for _, run := range []struct{ name, want string }{
		{"the change", `(?s).*row-table-index-settings-operation.*Schema apply completed successfully.*`},
		{"a run after the change", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, changed)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(heldSettings(c, opened, absolute), qt.DeepEquals, map[string]string{
		"by_body": "min=4 load=ENABLED", "by_n": "min=1 load=DISABLED replicas=PER_AZ:1"})
}

// An index added to a table that exists takes YDB's settings for a new index
// and then the declared ones, and an index renamed in place keeps its settings
// and takes a changed one under its new name, with one rename step rather than
// a drop and an add, as the host's embedded engine writes RENAME INDEX and
// then ALTER INDEX ... SET.
func TestHostIndexSettingsOnAddedAndRenamedIndexes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	output, err := hostApply(c, directory, indexSettingsItems(directory, "", "by_n", "", ""))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	added := indexSettingsItems(directory, "", "by_n", "", indexHolder("by_id_body", ` auto_partitioning_min_partitions_count="2" read_replicas_settings="PER_AZ:1"`))
	for _, run := range []struct{ name, source, want string }{
		{"the addition", added, `(?s).*row-table-alter-operation.*row-table-index-settings-operation.*Schema apply completed successfully.*`},
		{"a run after the addition", added, `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, run.source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(heldSettings(c, opened, absolute)["by_id_body"], qt.Equals, "min=2 load=DISABLED replicas=PER_AZ:1")

	renamed := indexSettingsItems(directory, "", "by_n", "", indexHolder("by_body_id", ` auto_partitioning_min_partitions_count="3" read_replicas_settings="PER_AZ:1"`))
	for _, run := range []struct{ name, want string }{
		{"the rename", `(?s).*row-table-alter-operation.*row-table-index-settings-operation.*Schema apply completed successfully.*`},
		{"a run after the rename", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApply(c, directory, renamed)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
		c.Assert(output, qt.Not(qt.Matches), `(?s).*Drops a secondary index.*`, qt.Commentf("%s", run.name))
	}
	held := heldSettings(c, opened, absolute)
	c.Assert(held["by_body_id"], qt.Equals, "min=3 load=DISABLED replicas=PER_AZ:1")
	_, kept := held["by_id_body"]
	c.Assert(kept, qt.IsFalse)
}

// A YAML index entry states the same settings with the same keys, as the
// host's TestParse_YDBGlobalIndex_HappyPath reads them.
func TestHostIndexSettingsFromYAML_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	source := "tables:\n  items:\n    schema: " + directory + "\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      body: {type: Utf8}\n" +
		"      n: {type: Int64}\n    indexes:\n      by_body:\n        fields: [body]\n        include: [n]\n        auto_partitioning_by_load: ENABLED\n" +
		"        auto_partitioning_min_partitions_count: 2\n      by_n:\n        fields: [n]\n        type: async\n        read_replicas_settings: PER_AZ:1\n"
	for _, run := range []struct{ name, want string }{
		{"the creation", `(?s).*row-table-index-settings-operation.*Schema apply completed successfully.*`},
		{"a second run", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApplySource(c, directory, "yaml", source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(heldSettings(c, opened, absolute), qt.DeepEquals, map[string]string{
		"by_body": "min=2 load=ENABLED", "by_n": "min=1 load=DISABLED replicas=PER_AZ:1"})
}

// A value an index setting cannot take is refused where the source wrote it,
// as the host's embedded engine refuses it, and nothing is applied. The host
// shows no reason for a refused Go decode; the unit tests pin the words.
func TestHostIndexSettingsFromGo_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	output, err := hostApply(c, directory, indexSettingsItems(directory, ` auto_partitioning_by_load="true"`, "by_n", "", ""))
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Matches, `(?s).*provider decode source failed \(InvalidArgument\) on //ptah:schema:index at Item.*`)
	c.Assert(tableExists(c, opened, absolute), qt.IsFalse)
}

// The host exports a table whose indexes' settings were changed with ALTER
// INDEX ... SET, as the host's TestYDBCompatBinary export tests build it: Go
// writes each changed setting on the index directive and HCL in the index
// block, as the host's embedded engine writes them, a setting a new index
// holds is left out, and either export applies back with nothing planned. An
// HCL document that changes a setting alters the index.
func TestHostExportsIndexSettings_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, absolute := itemsFixture(c)
	for _, statement := range []string{
		"CREATE TABLE `" + absolute + "` (id Uint64 NOT NULL, body Utf8, n Int64, PRIMARY KEY (id), INDEX by_body GLOBAL SYNC ON (body) COVER (n), INDEX by_n GLOBAL ASYNC ON (n))",
		"ALTER TABLE `" + absolute + "` ALTER INDEX by_body SET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, AUTO_PARTITIONING_BY_LOAD = ENABLED)",
		"ALTER TABLE `" + absolute + "` ALTER INDEX by_n SET (READ_REPLICAS_SETTINGS = 'PER_AZ:1')",
	} {
		c.Assert(execFixtureSchema(c.Context(), opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
	}
	written := introspectDirectory(c, directory)
	c.Assert(written, qt.Matches, `(?s).*//ptah:schema:index name="by_body" fields="body" include="n" table="[^"]*" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="2"\n.*`)
	c.Assert(written, qt.Matches, `(?s).*//ptah:schema:index name="by_n" fields="n" type="async" table="[^"]*" read_replicas_settings="PER_AZ:1"\n.*`)
	output, err := hostApply(c, directory, written)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)

	document, _ := hostExport(c, "schema", "inspect", "--schemas", directory, "--format", "hcl")
	c.Assert(document, qt.Matches, `(?s).*index "by_body" \{\n    include = \[column\.n\]\n    auto_partitioning_by_load = "ENABLED"\n    auto_partitioning_min_partitions_count = 2\n    columns = \[column\.body\]\n  \}.*`)
	c.Assert(document, qt.Matches, `(?s).*index "by_n" \{\n    type = "async"\n    read_replicas_settings = "PER_AZ:1"\n    columns = \[column\.n\]\n  \}.*`)
	for _, run := range []struct{ name, source, want string }{
		{"the HCL export", document, `(?s).*Schema is synced, no changes to be made.*`},
		{"a changed partition count", strings.Replace(document, "auto_partitioning_min_partitions_count = 2", "auto_partitioning_min_partitions_count = 3", 1),
			`(?s).*row-table-index-settings-operation.*Schema apply completed successfully.*`},
	} {
		output, err := hostApplySource(c, directory, "hcl", run.source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(heldSettings(c, opened, absolute)["by_body"], qt.Equals, "min=3 load=ENABLED")
}
