//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// storagePool is the storage pool kind local-ydb has, the only one its
// database takes: measured on 25.1.4.7 and 26.2.1.14, `ssd` and every other
// kind answer `database doesn't have required storage pools`.
const storagePool = "hdd"

// storedTable is what the server holds of a table's storage, as DescribeTable
// reports it: its TTL, each column family with the columns placed in it
// outside the default family, its splitting settings, its read replicas and
// its key bloom filter.
type storedTable struct {
	TTL            string
	Families       []string
	Partitioning   string
	ReadReplicas   string
	KeyBloomFilter string
}

// heldStorage reads the storage of the table at absolute back from the
// server, without the provider.
func heldStorage(c *qt.C, opened *connection.Connection, absolute string) storedTable {
	c.Helper()
	var held storedTable
	err := opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		described, err := session.DescribeTable(ctx, absolute)
		if err != nil {
			return err
		}
		held = storedTable{TTL: ttlText(described.TimeToLiveSettings), Families: familyTexts(described),
			Partitioning: partitioningText(described.PartitioningSettings), ReadReplicas: replicasText(described.ReadReplicaSettings),
			KeyBloomFilter: flagText(described.KeyBloomFilter)}
		return nil
	}, table.WithIdempotent())
	c.Assert(err, qt.IsNil)
	return held
}

// ttlText is a TTL as its column, its interval in seconds and, for an integer
// column, the unit its values count in, or empty for none.
func ttlText(ttl *options.TimeToLiveSettings) string {
	if ttl == nil {
		return ""
	}
	text := fmt.Sprintf("%s %ds", ttl.ColumnName, ttl.ExpireAfterSeconds)
	if ttl.Mode == options.TimeToLiveModeValueSinceUnixEpoch && ttl.ColumnUnit != nil {
		text += " " + map[options.TimeToLiveUnit]string{options.TimeToLiveUnitSeconds: "SECONDS", options.TimeToLiveUnitMilliseconds: "MILLISECONDS",
			options.TimeToLiveUnitMicroseconds: "MICROSECONDS", options.TimeToLiveUnitNanoseconds: "NANOSECONDS"}[*ttl.ColumnUnit]
	}
	return text
}

// familyTexts lists each family by name with its storage pool, its
// compression and the columns placed in it, in name order. The columns of the
// default family are not listed, as the host's reader does not list them.
func familyTexts(described options.Description) []string {
	placed := map[string][]string{}
	for _, column := range described.Columns {
		if column.Family != "" && column.Family != "default" {
			placed[column.Family] = append(placed[column.Family], column.Name)
		}
	}
	var families []string
	for _, family := range described.ColumnFamilies {
		text := family.Name
		if family.Data.Media != "" {
			text += " data=" + family.Data.Media
		}
		text += " compression=" + map[options.ColumnFamilyCompression]string{options.ColumnFamilyCompressionUnknown: "unspecified",
			options.ColumnFamilyCompressionNone: "off", options.ColumnFamilyCompressionLZ4: "lz4"}[family.Compression]
		if columns := placed[family.Name]; len(columns) != 0 {
			slices.Sort(columns)
			text += " columns=" + strings.Join(columns, ",")
		}
		families = append(families, text)
	}
	slices.Sort(families)
	return families
}

// partitioningText is a table's splitting settings, every one as the server
// reports it.
func partitioningText(settings options.PartitioningSettings) string {
	return fmt.Sprintf("by_size=%s size=%d by_load=%s min=%d max=%d", flagText(settings.PartitioningBySize), settings.PartitionSizeMb,
		flagText(settings.PartitioningByLoad), settings.MinPartitionsCount, settings.MaxPartitionsCount)
}

// replicasText is a table's read replicas as YQL writes them, or empty for
// none: a count of zero in either mode is none, which is how a table created
// without replicas reads, and how one reads after `PER_AZ:0` removed them.
func replicasText(settings options.ReadReplicasSettings) string {
	if settings.Count == 0 {
		return ""
	}
	switch settings.Type {
	case options.ReadReplicasPerAzReadReplicas:
		return fmt.Sprintf("PER_AZ:%d", settings.Count)
	case options.ReadReplicasAnyAzReadReplicas:
		return fmt.Sprintf("ANY_AZ:%d", settings.Count)
	}
	return ""
}

func flagText(flag options.FeatureFlag) string {
	return map[options.FeatureFlag]string{options.FeatureEnabled: "ENABLED", options.FeatureDisabled: "DISABLED"}[flag]
}

// emptyAtCleanup drops what a test created in the fixture directory once it
// ends, and keeps the directory for the fixture's own cleanup.
func emptyAtCleanup(c *qt.C, opened *connection.Connection, absolute string) {
	c.Helper()
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		emptyTree(ctx, opened, absolute)
	})
}

// rowCount counts the rows of the table at absolute that match the condition.
func rowCount(c *qt.C, opened *connection.Connection, absolute, condition string) int64 {
	c.Helper()
	query := "SELECT COUNT(*) FROM `" + absolute + "`"
	if condition != "" {
		query += " WHERE " + condition
	}
	var count int64
	c.Assert(opened.DB.QueryRowContext(c.Context(), query).Scan(&count), qt.IsNil, qt.Commentf("%s", query))
	return count
}

// applyUntilSynced applies source through the host and requires that the
// first run changes the database and the next two find nothing to change, as
// the host's round trips plan, apply, and plan again twice.
func applyUntilSynced(c *qt.C, selectors, format, source string, flags ...string) {
	c.Helper()
	for _, run := range []struct{ name, want string }{
		{"the first run", `(?s).*Schema apply completed successfully.*`},
		{"a second run", `(?s).*Schema is synced, no changes to be made.*`},
		{"a third run", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApplySelected(c, selectors, format, source, flags...)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s: %s", run.name, output))
	}
}

// ttlTables declares a table whose TTL reads a date column and one whose TTL
// reads an integer column in milliseconds.
const ttlTables = `package entities

//ptah:schema:table name="events" schema="{dir}" platform.ydb.row_deletion_column="created_at" platform.ydb.row_deletion_interval="PT720H"
type Event struct {
	//ptah:schema:field name="id" type="Int64" primary="true" not_null="true"
	ID int64
	//ptah:schema:field name="created_at" type="Timestamp"
	CreatedAt int64
}

//ptah:schema:table name="tokens" schema="{dir}" platform.ydb.row_deletion_column="expires" platform.ydb.row_deletion_interval="PT1H" platform.ydb.row_deletion_unit="MILLISECONDS"
type Token struct {
	//ptah:schema:field name="id" type="Int64" primary="true" not_null="true"
	ID int64
	//ptah:schema:field name="expires" type="Uint64"
	Expires uint64
}
`

// Ported from the host's TestYDBTTL_RoundTrip: the host creates a table whose
// TTL reads a date column and one whose TTL reads an integer column with a
// unit, finds nothing to change on the next two runs, and the server holds
// both TTLs as declared, the 720 hours read back as 30 days. The host's test
// pins the CREATE TABLE text; the provider creates the table through the
// table service, so the server's description is the measure.
func TestHostCreatesTablesWithATTL_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))

	applyUntilSynced(c, directory, "go", replaceDirectory(ttlTables, directory))

	c.Assert(heldStorage(c, opened, path.Join(database, directory, "events")).TTL, qt.Equals, "created_at 2592000s")
	c.Assert(heldStorage(c, opened, path.Join(database, directory, "tokens")).TTL, qt.Equals, "expires 3600s MILLISECONDS")
}

// familyDocs declares table docs with a key, a NOT NULL column with a
// default and two nullable columns, with the families given before its
// struct and the further columns given after its own.
func familyDocs(families, extra string) string {
	return "package entities\n\n//ptah:schema:table name=\"docs\" schema=\"{dir}\"\n" + families + "type Doc struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Int64\" primary=\"true\" not_null=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"title\" type=\"Utf8\" not_null=\"true\" default=\"untitled\"\n\tTitle string\n" +
		"\t//ptah:schema:field name=\"body\" type=\"Utf8\"\n\tBody string\n" +
		"\t//ptah:schema:field name=\"blob\" type=\"String\"\n\tBlob []byte\n" + extra + "}\n"
}

// Ported from the host's TestYDBColumnFamilies_RoundTrip, without the cache
// mode the host gives family hot on a line that takes one (gap G10): the host
// creates a table whose default family is compressed, whose family cold is
// on a storage pool and holds two columns, whose family hot states no setting
// and holds the NOT NULL column, and whose family spare holds no column. The
// next two runs find nothing to change, and the server holds every family
// with its columns and settings, a family stating no compression read back
// as off.
func TestHostCreatesATableWithColumnFamilies_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	families := "//ptah:schema:columnfamily name=\"default\" compression=\"lz4\"\n" +
		"//ptah:schema:columnfamily name=\"cold\" data=\"" + storagePool + "\" compression=\"lz4\" fields=\"body,blob\"\n" +
		"//ptah:schema:columnfamily name=\"hot\" fields=\"title\"\n//ptah:schema:columnfamily name=\"spare\"\n"

	applyUntilSynced(c, directory, "go", replaceDirectory(familyDocs(families, ""), directory))

	c.Assert(heldStorage(c, opened, path.Join(database, directory, "docs")).Families, qt.DeepEquals, []string{
		"cold data=hdd compression=lz4 columns=blob,body",
		"default compression=lz4",
		"hot compression=off columns=title",
		"spare compression=off",
	})
}

// Ported from the host's TestYDBColumnFamilies_KeepInMemoryIsRefusedWithDefaultFlags:
// keep_in_memory is the one family setting the table service could set that
// YQL has no spelling for, and with default flags YDB refuses it on both
// lines, so no plan of the provider's can be asked to keep it. The families
// stay as the host created them.
func TestKeepInMemoryIsRefusedWithDefaultFlagsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "docs")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	created, err := hostApplySource(c, directory, "go", replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"cold\" fields=\"body\"\n", ""), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))

	err = opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		return session.AlterTable(ctx, absolute, options.WithAlterColumnFamilies(options.ColumnFamily{Name: "cold", KeepInMemory: options.FeatureEnabled}))
	})

	c.Assert(err, qt.ErrorMatches, `(?s).*BAD_REQUEST.*Setting keep_in_memory to ENABLED is not allowed.*`)
	c.Assert(heldStorage(c, opened, absolute).Families, qt.DeepEquals, []string{"cold compression=off columns=body", "default compression=off"})
}

// partitionedItems declares table items keyed on an unsigned id and a text
// code, with the table attributes given.
func partitionedItems(attributes string) string {
	return "package entities\n\n//ptah:schema:table name=\"items\" schema=\"{dir}\" primary_key=\"id,code\"" + attributes + "\ntype Item struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"Uint64\" not_null=\"true\"\n\tID uint64\n" +
		"\t//ptah:schema:field name=\"code\" type=\"Utf8\" not_null=\"true\"\n\tCode string\n" +
		"\t//ptah:schema:field name=\"label\" type=\"Utf8\"\n\tLabel string\n}\n"
}

// Ported from the host's TestYDBTablePartitioning_StartingLayout: the host
// creates a table with each starting layout and plans nothing after. YDB
// keeps no record of the layout, and the minimum partition count it set
// reads back as the one the declaration gives a new table; the split points
// are written in the key's own types, a text value with a quote on the
// second key column included.
func TestHostCreatesATableWithAStartingLayout_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, attributes, want string }{
		{name: "uniform partitions", attributes: ` uniform_partitions="4"`, want: "by_size=ENABLED size=2048 by_load=DISABLED min=4 max=0"},
		{name: "split points", attributes: ` partition_at_keys="(10, 'it\\'s'), (20)"`, want: "by_size=ENABLED size=2048 by_load=DISABLED min=3 max=0"},
		{name: "a layout beside a declared minimum", attributes: ` uniform_partitions="4" auto_partitioning_min_partitions_count="2" auto_partitioning_by_size="DISABLED"`,
			want: "by_size=DISABLED size=0 by_load=DISABLED min=2 max=0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			emptyAtCleanup(c, opened, path.Join(database, directory))

			applyUntilSynced(c, directory, "go", replaceDirectory(partitionedItems(test.attributes), directory))

			c.Assert(heldStorage(c, opened, path.Join(database, directory, "items")).Partitioning, qt.Equals, test.want)
		})
	}
}

// Ported from the host's TestYDBTablePartitioning_ClusterProfileIsKept: a new
// table takes its settings from the cluster's table profile, not from YDB's
// documentation, and with the cluster's configuration replaced a new table
// does not split by size. A declaration that names no setting keeps that and
// plans nothing; declaring AUTO_PARTITIONING_BY_SIZE = ENABLED turns it on and
// moves nothing else, and nothing is left to plan once it ran. The flags this
// test replaces are restored when it ends.
func TestHostKeepsTheClusterTableProfile_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableMoveIndex": false})
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	silent := replaceDirectory(partitionedItems(""), directory)

	created, err := hostApplySource(c, directory, "go", silent)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(heldStorage(c, opened, absolute).Partitioning, qt.Equals, "by_size=DISABLED size=0 by_load=DISABLED min=1 max=0")
	c.Assert(hostPlanned(c, directory, "go", silent), qt.HasLen, 0)

	bySize := replaceDirectory(partitionedItems(` auto_partitioning_by_size="ENABLED"`), directory)
	output, err := hostApplySource(c, directory, "go", bySize)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*row-table-alter-operation.*Schema apply completed successfully.*`)
	c.Assert(hostPlanned(c, directory, "go", bySize), qt.HasLen, 0)
	c.Assert(hostPlanned(c, directory, "go", silent), qt.HasLen, 0)
	c.Assert(heldStorage(c, opened, absolute).Partitioning, qt.Equals, "by_size=ENABLED size=2048 by_load=DISABLED min=1 max=0")
}

// Ported from the host's TestYDBColumnFamilies_WhatTheDeclarationLeavesOutStays:
// a family and a storage pool the declaration leaves out stay, since a
// cluster's table profile can give them to every new table and YQL drops
// neither. A Go source states where every column sits, so the column a
// left-out family held moves back to the default family, in one alteration.
// The plan is the same with a rebuild allowed and without one, it rebuilds
// nothing, and the table keeps the family, the pool and the row.
func TestHostKeepsWhatAColumnFamilyDeclarationLeavesOut_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, families string
		// moves is whether the plan moves a column, in one alteration of docs.
		moves    bool
		wantRead []string
	}{
		{name: "a family left out", families: "//ptah:schema:columnfamily name=\"cold\" data=\"" + storagePool + "\" fields=\"body\"\n", moves: true,
			wantRead: []string{"cold data=hdd compression=off columns=body", "default compression=off", "warm compression=off"}},
		{name: "a pool left out", families: "//ptah:schema:columnfamily name=\"cold\" fields=\"body\"\n//ptah:schema:columnfamily name=\"warm\" fields=\"blob\"\n",
			wantRead: []string{"cold data=hdd compression=off columns=body", "default compression=off", "warm compression=off columns=blob"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			absolute := path.Join(database, directory, "docs")
			emptyAtCleanup(c, opened, path.Join(database, directory))
			created, err := hostApplySource(c, directory, "go", replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"cold\" data=\""+storagePool+
				"\" fields=\"body\"\n//ptah:schema:columnfamily name=\"warm\" fields=\"blob\"\n", ""), directory))
			c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
			_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, title, body) VALUES (1l, 'a'u, 'one'u)")
			c.Assert(err, qt.IsNil)
			source := replaceDirectory(familyDocs(test.families, ""), directory)
			want := map[bool][]string{true: {"row-table-alter-operation " + directory + ".docs"}, false: {}}[test.moves]

			c.Assert(hostPlanned(c, directory, "go", source, "--allow-table-rebuild"), qt.DeepEquals, hostPlanned(c, directory, "go", source))
			c.Assert(hostPlanned(c, directory, "go", source), qt.DeepEquals, want)
			output, err := hostApplySource(c, directory, "go", source)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(hostPlanned(c, directory, "go", source), qt.HasLen, 0)
			c.Assert(heldStorage(c, opened, absolute).Families, qt.DeepEquals, test.wantRead)
			c.Assert(rowCount(c, opened, absolute, "body = 'one'u"), qt.Equals, int64(1))
		})
	}
}

// Ported from the host's TestYDBColumnFamilies_ChangesInPlace: on a table
// holding rows, a family is added holding a column added and one moved;
// then settings change, a pool is given, a column goes back to the default
// family because the declaration places it in no family, and another column
// is dropped; then the declaration leaves out every setting and two families
// and finds nothing to change, since the table keeps what it holds. Each run
// leaves nothing to plan, the server holds the families the step names, and
// the rows stay. The host's test pins the statements; the server accepting
// them in one run and the read after it is the measure here, since the
// provider runs them as native operations.
func TestHostChangesColumnFamiliesInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "docs")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	note := "\t//ptah:schema:field name=\"note\" type=\"Utf8\"\n\tNote string\n"
	created, err := hostApplySource(c, directory, "go",
		replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"cold\" compression=\"lz4\" fields=\"body,blob\"\n", ""), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, title, body, blob) VALUES (1l, 'a'u, 'one'u, 'x'), (2l, 'b'u, NULL, NULL)")
	c.Assert(err, qt.IsNil)

	for _, step := range []struct {
		name, families, extra, want string
		wantRead                    []string
	}{
		{name: "a family added, holding a column added and one moved",
			families: "//ptah:schema:columnfamily name=\"cold\" compression=\"lz4\" fields=\"body\"\n//ptah:schema:columnfamily name=\"warm\" fields=\"blob,note\"\n",
			extra:    note, want: `(?s).*Schema apply completed successfully.*`,
			wantRead: []string{"cold compression=lz4 columns=body", "default compression=off", "warm compression=off columns=blob,note"}},
		{name: "settings changed, a pool given, a column back in the default family, another dropped",
			families: "//ptah:schema:columnfamily name=\"default\" compression=\"lz4\"\n" +
				"//ptah:schema:columnfamily name=\"cold\" data=\"" + storagePool + "\" compression=\"off\" fields=\"blob\"\n" +
				"//ptah:schema:columnfamily name=\"warm\" compression=\"lz4\"\n",
			want:     `(?s).*Schema apply completed successfully.*`,
			wantRead: []string{"cold data=hdd compression=off columns=blob", "default compression=lz4", "warm compression=lz4"}},
		{name: "settings and families left out", families: "//ptah:schema:columnfamily name=\"cold\" fields=\"blob\"\n",
			want:     `(?s).*Schema is synced, no changes to be made.*`,
			wantRead: []string{"cold data=hdd compression=off columns=blob", "default compression=lz4", "warm compression=lz4"}},
	} {
		source := replaceDirectory(familyDocs(step.families, step.extra), directory)
		output, err := hostApplySource(c, directory, "go", source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", step.name, output))
		c.Assert(output, qt.Matches, step.want, qt.Commentf("%s: %s", step.name, output))
		c.Assert(hostPlanned(c, directory, "go", source), qt.HasLen, 0, qt.Commentf("%s", step.name))
		c.Assert(heldStorage(c, opened, absolute).Families, qt.DeepEquals, step.wantRead, qt.Commentf("%s", step.name))
	}
	c.Assert(rowCount(c, opened, absolute, "body = 'one'u AND blob = 'x'"), qt.Equals, int64(1))
	c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(2))
}
