//go:build integration

package integration_test

import (
	"context"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Table_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// heldCacheModes reads the cache mode of each family of the table at
// absolute that holds one, regular or in_memory, straight from the table
// service: the SDK's description has no field for it, and the pinned
// bindings keep field 6 of a family, its cache mode, as an unknown field.
func heldCacheModes(c *qt.C, opened *connection.Connection, absolute string) map[string]string {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := Ydb_Table_V1.NewTableServiceClient(ydb.GRPCConn(opened.Driver))
	created, err := client.CreateSession(ctx, &Ydb_Table.CreateSessionRequest{})
	c.Assert(err, qt.IsNil)
	var session Ydb_Table.CreateSessionResult
	c.Assert(created.GetOperation().GetResult().UnmarshalTo(&session), qt.IsNil)
	defer func() {
		_, err := client.DeleteSession(ctx, &Ydb_Table.DeleteSessionRequest{SessionId: session.SessionId})
		c.Check(err, qt.IsNil)
	}()
	described, err := client.DescribeTable(ctx, &Ydb_Table.DescribeTableRequest{SessionId: session.SessionId, Path: absolute})
	c.Assert(err, qt.IsNil)
	c.Assert(described.GetOperation().GetStatus(), qt.Equals, Ydb.StatusIds_SUCCESS, qt.Commentf("%v", described.GetOperation().GetIssues()))
	var table Ydb_Table.DescribeTableResult
	c.Assert(described.GetOperation().GetResult().UnmarshalTo(&table), qt.IsNil)
	modes := make(map[string]string)
	for _, family := range table.ColumnFamilies {
		unknown := family.ProtoReflect().GetUnknown()
		for len(unknown) > 0 {
			number, wireType, length := protowire.ConsumeTag(unknown)
			c.Assert(length > 0 && number == 6 && wireType == protowire.VarintType, qt.IsTrue, qt.Commentf("family %s holds unknown fields %x", family.Name, unknown))
			value, valueLength := protowire.ConsumeVarint(unknown[length:])
			c.Assert(valueLength > 0, qt.IsTrue)
			modes[family.Name] = map[uint64]string{1: "regular", 2: "in_memory"}[value]
			unknown = unknown[length+valueLength:]
		}
	}
	return modes
}

// targetTakesCacheModes reports whether a provider session on the test server
// takes a family cache mode: 25.4 and later with default flags, or a cluster
// that turned EnableTableCacheModes on.
func targetTakesCacheModes(c *qt.C) bool {
	c.Helper()
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return session.Capabilities[tablecreate.CacheModeCapability]
}

// requireCacheModes skips a test on a target whose answer is not the one the
// test is about.
func requireCacheModes(c *qt.C, wanted bool) {
	c.Helper()
	if targetTakesCacheModes(c) != wanted {
		c.Skipf("the target's %s is not %t", tablecreate.CacheModeCapability, wanted)
	}
}

// cacheModeRefusal is how the host reports a declared cache mode on a target
// without the capability: the provider's words, which name the flag YDB keeps
// it behind.
const cacheModeRefusal = `(?s).*declares column family cache mode, which requires target capability column_family_cache_mode, unavailable on this ydb target; ` +
	`YDB keeps it behind feature flag EnableTableCacheModes: turn the flag on, and name the cluster's monitoring endpoint in the URL.*`

// On a line that takes a cache mode, a family's cache mode changes in place
// on a table holding rows: one alteration sets it on a family the table holds
// and adds a family with one, holding a column it moves; the next changes
// both; and a declaration that leaves the cache modes out plans nothing, since
// the table keeps what it holds. Each run leaves nothing to plan, the server
// holds the cache modes the step names, and the rows stay.
func TestHostChangesAFamilyCacheModeInPlace_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireCacheModes(c, true)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "docs")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	created, err := hostApplySource(c, directory, "go", replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"hot\" fields=\"title\"\n", ""), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	_, err = opened.DB.ExecContext(c.Context(), "UPSERT INTO `"+absolute+"` (id, title, body) VALUES (1l, 'a'u, 'one'u), (2l, 'b'u, NULL)")
	c.Assert(err, qt.IsNil)

	for _, step := range []struct {
		name, families string
		wantPlan       []string
		wantFamilies   []string
		wantModes      map[string]string
	}{
		{name: "a cache mode set and a family added with one",
			families: "//ptah:schema:columnfamily name=\"hot\" cache_mode=\"in_memory\" fields=\"title\"\n" +
				"//ptah:schema:columnfamily name=\"warm\" cache_mode=\"regular\" fields=\"body\"\n",
			wantPlan:     []string{"row-table-alter-operation " + directory + ".docs"},
			wantFamilies: []string{"default compression=off", "hot compression=off columns=title", "warm compression=off columns=body"},
			wantModes:    map[string]string{"hot": "in_memory", "warm": "regular"}},
		{name: "both cache modes changed",
			families: "//ptah:schema:columnfamily name=\"hot\" cache_mode=\"REGULAR\" fields=\"title\"\n" +
				"//ptah:schema:columnfamily name=\"warm\" cache_mode=\"in_memory\" fields=\"body\"\n",
			wantPlan:     []string{"row-table-alter-operation " + directory + ".docs"},
			wantFamilies: []string{"default compression=off", "hot compression=off columns=title", "warm compression=off columns=body"},
			wantModes:    map[string]string{"hot": "regular", "warm": "in_memory"}},
		{name: "the cache modes left out",
			families:     "//ptah:schema:columnfamily name=\"hot\" fields=\"title\"\n//ptah:schema:columnfamily name=\"warm\" fields=\"body\"\n",
			wantPlan:     make([]string, 0),
			wantFamilies: []string{"default compression=off", "hot compression=off columns=title", "warm compression=off columns=body"},
			wantModes:    map[string]string{"hot": "regular", "warm": "in_memory"}},
	} {
		source := replaceDirectory(familyDocs(step.families, ""), directory)
		c.Assert(hostPlanned(c, directory, "go", source), qt.DeepEquals, step.wantPlan, qt.Commentf("%s", step.name))
		output, err := hostApplySource(c, directory, "go", source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", step.name, output))
		c.Assert(hostPlanned(c, directory, "go", source), qt.HasLen, 0, qt.Commentf("%s", step.name))
		c.Assert(heldStorage(c, opened, absolute).Families, qt.DeepEquals, step.wantFamilies, qt.Commentf("%s", step.name))
		c.Assert(heldCacheModes(c, opened, absolute), qt.DeepEquals, step.wantModes, qt.Commentf("%s", step.name))
	}
	c.Assert(rowCount(c, opened, absolute, "title = 'a'u AND body = 'one'u"), qt.Equals, int64(1))
	c.Assert(rowCount(c, opened, absolute, ""), qt.Equals, int64(2))
}

// On a line without a cache mode a declared one is refused before anything
// runs, in a new table and in one the table already holds, regular included,
// and the refusal names the flag: the table service of 25.1.4.7 does not know
// the field, and would create or alter the table without it.
func TestHostRefusesAFamilyCacheModeTheTargetLacks_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	requireCacheModes(c, false)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "docs")
	emptyAtCleanup(c, opened, path.Join(database, directory))

	refused, err := hostApplySource(c, directory, "go", replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"hot\" cache_mode=\"in_memory\" fields=\"title\"\n", ""), directory))
	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.Matches, cacheModeRefusal)
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	c.Assert(listing.Children, qt.HasLen, 0)

	created, err := hostApplySource(c, directory, "go", replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"hot\" fields=\"title\"\n", ""), directory))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", created))
	refused, err = hostApplySource(c, directory, "yaml", replaceDirectory("tables:\n  docs:\n    schema: \"{dir}\"\n    column_families:\n      hot: {cache_mode: regular, fields: [title]}\n"+
		"    columns:\n      id: {type: Int64, primary: true, not_null: true}\n      title: {type: Utf8, not_null: true, default: untitled}\n      body: {type: Utf8}\n      blob: {type: String}\n", directory))
	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.Matches, cacheModeRefusal)
	c.Assert(heldStorage(c, opened, absolute).Families, qt.DeepEquals, []string{"default compression=off", "hot compression=off columns=title"})
	c.Assert(heldCacheModes(c, opened, absolute), qt.DeepEquals, make(map[string]string))
}

// On a line that takes a cache mode by default, a cluster can turn
// EnableTableCacheModes off, as 25.3 has it by default (`Setting cache_mode is
// not allowed`, as the host measured there). A session that read the flags
// then reports no cache mode, and the declaration is refused with the flag's
// name before anything runs. The flags are restored when the test ends.
func TestHostRefusesAFamilyCacheModeTheClusterTurnedOff_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	requireCacheModes(c, true)
	opened, directory, database := lockFixture(c)
	emptyAtCleanup(c, opened, path.Join(database, directory))
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableTableCacheModes": false})
	c.Assert(targetTakesCacheModes(c), qt.IsFalse)

	refused, err := hostApplySource(c, directory, "go", replaceDirectory(familyDocs("//ptah:schema:columnfamily name=\"hot\" cache_mode=\"in_memory\" fields=\"title\"\n", ""), directory))

	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.Matches, cacheModeRefusal)
	listing, err := opened.Driver.Scheme().ListDirectory(c.Context(), path.Join(database, directory))
	c.Assert(err, qt.IsNil)
	c.Assert(listing.Children, qt.HasLen, 0)
}

// A table that holds a cache mode is read, written back as a Go file or a
// YAML document with the family's cache_mode, and the source applied back
// through the host plans nothing.
func TestHostReplansAnExportedCacheModeSubprocess(t *testing.T) {
	for _, test := range []struct {
		format     protocol.SourceDocument_Format
		name, want string
	}{
		{protocol.SourceDocument_GO_ANNOTATIONS, "go", `(?s).*//ptah:schema:columnfamily name="hot" compression="off" cache_mode="in_memory" fields="body".*`},
		{protocol.SourceDocument_YAML, "yaml", `(?s).*"hot": \{"compression": "off", "cache_mode": "in_memory", "fields": \["body"\]\}.*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			requireCacheModes(c, true)
			opened, directory, database := lockFixture(c)
			emptyAtCleanup(c, opened, path.Join(database, directory))
			c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TABLE `"+path.Join(database, directory, "docs")+"` (id Uint64 NOT NULL, body Utf8 FAMILY hot, "+
				"PRIMARY KEY (id), FAMILY hot (CACHE_MODE = 'in_memory'))"), qt.IsNil)
			read := readDirectory(c, directory)
			request := exportRequest(read, test.format)
			encoded, err := read.client.EncodeSource(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(encoded.Losses, qt.HasLen, 0, qt.Commentf("%v", encoded.Losses))
			source := map[protocol.SourceDocument_Format]func() string{
				protocol.SourceDocument_GO_ANNOTATIONS: func() string { return renderGo(c, read, request, encoded) },
				protocol.SourceDocument_YAML:           func() string { return renderYAML(read, request, encoded) },
			}[test.format]()
			c.Assert(source, qt.Matches, test.want)
			output, err := hostReplanSource(c, directory, test.name, source)
			c.Assert(err, qt.IsNil, qt.Commentf("%s\n%s", source, output))
			c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`, qt.Commentf("%s\n%s", source, output))
		})
	}
}
