//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Coordination_V1"
	"github.com/ydb-platform/ydb-go-genproto/Ydb_Scheme_V1"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	ydb "github.com/ydb-platform/ydb-go-sdk/v3"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeapi"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// streamingQueryEntry is the scheme entry type of a streaming query, which
// the pinned vendor protobuf does not name.
const streamingQueryEntry = Ydb_Scheme.Entry_Type(26)

// newDirectorySecret is the variable the secret a source declares in a new
// directory takes its value from.
const newDirectorySecret = "PTAH_SECRET_NEW_DIRECTORY" // #nosec G101 -- the name of the variable that carries the value

// heldEntries lists the directory at absolute through the scheme service, as
// names and entry types. A directory YDB does not show lists as nil.
func heldEntries(c *qt.C, opened *connection.Connection, absolute string) map[string]Ydb_Scheme.Entry_Type {
	c.Helper()
	listed, err := nativeapi.SchemeLister(Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(opened.Driver)))(c.Context(), absolute)
	if err != nil {
		c.Assert(err, qt.ErrorIs, nativeapi.ErrDirectoryNotVisible)
		return nil
	}
	held := make(map[string]Ydb_Scheme.Entry_Type)
	for _, child := range listed.GetChildren() {
		held[child.GetName()] = child.GetType()
	}
	return held
}

// removeTree drops every object below absolute, dependents first, and then
// removes the directories, deepest first. It removes only what a test
// created there.
func removeTree(ctx context.Context, opened *connection.Connection, absolute string) {
	emptyTree(ctx, opened, absolute)
	_ = opened.Driver.Scheme().RemoveDirectory(ctx, absolute)
}

// emptyTree is removeTree that keeps absolute itself, for a fixture
// directory whose own cleanup removes it.
func emptyTree(ctx context.Context, opened *connection.Connection, absolute string) {
	scheme := nativeapi.SchemeLister(Ydb_Scheme_V1.NewSchemeServiceClient(ydb.GRPCConn(opened.Driver)))
	nodes := Ydb_Coordination_V1.NewCoordinationServiceClient(ydb.GRPCConn(opened.Driver))
	drops := []struct {
		kind Ydb_Scheme.Entry_Type
		verb string
	}{
		{streamingQueryEntry, "DROP STREAMING QUERY"}, {Ydb_Scheme.Entry_VIEW, "DROP VIEW"}, {Ydb_Scheme.Entry_EXTERNAL_TABLE, "DROP EXTERNAL TABLE"},
		{Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE, "DROP EXTERNAL DATA SOURCE"}, {Ydb_Scheme.Entry_TOPIC, "DROP TOPIC"}, {Ydb_Scheme.Entry_SECRET, "DROP SECRET"},
		{Ydb_Scheme.Entry_TABLE, "DROP TABLE"}, {Ydb_Scheme.Entry_COLUMN_TABLE, "DROP TABLE"},
	}
	directories := []string{absolute}
	for i := 0; i < len(directories); i++ {
		listed, err := scheme(ctx, directories[i])
		if err != nil {
			continue
		}
		for _, drop := range drops {
			for _, child := range listed.GetChildren() {
				if child.GetType() == drop.kind {
					_ = execFixtureSchema(ctx, opened.DB, drop.verb+" `"+path.Join(directories[i], child.GetName())+"`")
				}
			}
		}
		for _, child := range listed.GetChildren() {
			switch child.GetType() {
			case Ydb_Scheme.Entry_COORDINATION_NODE:
				_, _ = nodes.DropNode(ctx, &Ydb_Coordination.DropNodeRequest{Path: path.Join(directories[i], child.GetName())})
			case Ydb_Scheme.Entry_DIRECTORY:
				directories = append(directories, path.Join(directories[i], child.GetName()))
			}
		}
	}
	slices.Reverse(directories)
	for _, directory := range directories[:len(directories)-1] {
		_ = opened.Driver.Scheme().RemoveDirectory(ctx, directory)
	}
}

// targetCapabilities are the capabilities a provider session establishes on
// the test address.
func targetCapabilities(c *qt.C) map[string]bool {
	c.Helper()
	client := startProviderFor(c)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Assert(client.CloseSession(c.Context(), session.Id), qt.IsNil)
	return session.Capabilities
}

// requireCapability skips a kind the target does not keep: secrets and
// streaming queries exist on 26.2 and not on 25.1. An empty capability is
// one every target keeps.
func requireCapability(c *qt.C, capabilities map[string]bool, capability string) {
	c.Helper()
	if capability != "" && !capabilities[capability] {
		c.Skipf("the target does not keep %s", capability)
	}
}

// The host creates the first objects of a directory that does not exist yet,
// two levels below the fixture, through a run filtered to that directory, and
// finds them as declared on the next run. The read finds the directory empty,
// because its parent's listing does not name it, each operation's read before
// its write finds no occupant there on the same evidence, and YDB creates the
// missing directories with the object. The host's run takes the schema lock
// at the database root, whose node the lock fixture removes.
func TestHostCreatesTheFirstObjectOfANewDirectory_HappyPath(t *testing.T) {
	capabilities := targetCapabilities(testcheck.New(t))
	for _, test := range []struct {
		name, capability, format, source string
		created                          map[string]Ydb_Scheme.Entry_Type
	}{
		{name: "a topic", format: "yaml", source: "topics:\n  events:\n    schema: {dir}\n    min_active_partitions: 1\n",
			created: map[string]Ydb_Scheme.Entry_Type{"events": Ydb_Scheme.Entry_TOPIC}},
		{name: "a coordination node", format: "yaml", source: "coordination_nodes:\n  workers:\n    schema: {dir}\n    read_consistency_mode: strict\n",
			created: map[string]Ydb_Scheme.Entry_Type{"workers": Ydb_Scheme.Entry_COORDINATION_NODE}},
		{name: "external objects", format: "yaml",
			source: "external_data_sources:\n  bucket:\n    schema: {dir}\n    source_type: ObjectStorage\n    location: https://storage.example.test/events/\n    auth_method: NONE\n" +
				"external_tables:\n  events:\n    schema: {dir}\n    data_source: {dir}/bucket\n    location: 2026/\n    columns:\n      - {name: id, type: Int64, not_null: true}\n" +
				"    options:\n      FORMAT: csv_with_names\n",
			created: map[string]Ydb_Scheme.Entry_Type{"bucket": Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE, "events": Ydb_Scheme.Entry_EXTERNAL_TABLE}},
		{name: "a row table", format: "yaml", source: "tables:\n  items:\n    schema: {dir}\n    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n",
			created: map[string]Ydb_Scheme.Entry_Type{"items": Ydb_Scheme.Entry_TABLE}},
		{name: "a column table", format: "go", source: hostEvents("{dir}", ` store="COLUMN" partition_by_hash="id"`),
			created: map[string]Ydb_Scheme.Entry_Type{"events": Ydb_Scheme.Entry_COLUMN_TABLE}},
		{name: "a secret", capability: "secrets", format: "yaml", source: "secrets:\n  pg.pw:\n    schema: {dir}\n    value_env: " + newDirectorySecret + "\n",
			created: map[string]Ydb_Scheme.Entry_Type{"pg.pw": Ydb_Scheme.Entry_SECRET}},
		{name: "a streaming query", capability: streaming.Capability, format: "yaml",
			source: "topics:\n  source:\n    schema: {dir}\n  sink:\n    schema: {dir}\nstreaming_queries:\n  copy:\n    schema: {dir}\n" +
				"    text: \"INSERT INTO `{abs}/sink` SELECT * FROM `{abs}/source`;\"\n    run: false\n",
			created: map[string]Ydb_Scheme.Entry_Type{"source": Ydb_Scheme.Entry_TOPIC, "sink": Ydb_Scheme.Entry_TOPIC, "copy": streamingQueryEntry}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			requireCapability(c, capabilities, test.capability)
			c.Setenv(newDirectorySecret, "synthetic-new-directory-value")
			opened, directory, database := lockFixture(c)
			fresh := path.Join(directory, "fresh", "nested")
			c.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				removeTree(ctx, opened, path.Join(database, directory, "fresh"))
			})
			source := strings.NewReplacer("{dir}", fresh, "{abs}", path.Join(database, fresh)).Replace(test.source)
			c.Assert(heldEntries(c, opened, path.Join(database, directory, "fresh")), qt.IsNil)
			output, err := hostApplySelected(c, fresh, test.format, source)
			c.Assert(err, qt.IsNil, qt.Commentf("creation: %s", output))
			c.Assert(output, qt.Matches, `(?s).*Schema apply completed successfully.*`)
			c.Assert(heldEntries(c, opened, path.Join(database, fresh)), qt.DeepEquals, test.created)
			output, err = hostApplySelected(c, fresh, test.format, source)
			c.Assert(err, qt.IsNil, qt.Commentf("a second run: %s", output))
			c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
		})
	}
}

// hostApplyEverywhere runs the host's `schema apply` over one YAML schema file
// against address with no schema selector, so the read lists every directory
// of the session's database.
func hostApplyEverywhere(c *qt.C, address, text string) (string, error) {
	c.Helper()
	return hostApplyEverywhereAs(c, address, "schema.yaml", text)
}

// hostApplyEverywhereAs is hostApplyEverywhere over a schema file of the name
// given, whose extension says its format, such as schema.sql for YQL.
func hostApplyEverywhereAs(c *qt.C, address, name, text string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	file := filepath.Join(c.TempDir(), name)
	c.Assert(os.WriteFile(file, []byte(text), 0o600), qt.IsNil)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := testCommand(ctx, host, "schema", "apply", "--auto-approve", "--tx-mode", "none", "--provider-config", manifest, "--provider-engine", "ydb",
		"--db-url", address, "--schema-file", file)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

// A run with no schema selector creates a topic in a directory that does not
// exist yet. The read lists every directory the walk reaches and claims the
// root and every directory below it complete in one region, so the topic's
// absence two levels below the root is established although no listing named
// its directory: the host plans the topic, YDB creates the missing
// directories with it, and the next run plans nothing. The run is confined to
// a dev realm of its own: an unfiltered run on the shared test database would
// read, and plan to drop, every other run's objects. The run's schema lock is
// a node at the realm's root, which the cleanup removes before the realm.
func TestHostCreatesInANewDirectoryWithoutASelector_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, _, _ := lockFixture(c)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	address, root := realmFixture(c, opened, base)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		removeTree(ctx, opened, path.Join(root, "fresh"))
		_ = opened.Driver.Coordination().DropNode(ctx, path.Join(root, locking.NodeName))
	})
	const source = "topics:\n  events:\n    schema: fresh/nested\n    min_active_partitions: 1\n"

	created, createErr := hostApplyEverywhere(c, address, source)
	held := heldEntries(c, opened, path.Join(root, "fresh", "nested"))
	again, againErr := hostApplyEverywhere(c, address, source)

	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(created, qt.Not(qt.Contains), "absence was not established")
	c.Assert(created, qt.Contains, "Schema apply completed successfully")
	c.Assert(held, qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"events": Ydb_Scheme.Entry_TOPIC})
	c.Assert(againErr, qt.IsNil, qt.Commentf("%s", again))
	c.Assert(again, qt.Contains, "Schema is synced, no changes to be made")
}

// A run with no schema selector creates a topic in a directory whose name
// starts with a dot, as the host's embedded engine creates it. Neither reads
// such a directory: it holds nothing the read describes, so the root's
// complete claim establishes the topic's absence there.
func TestHostCreatesInADotPrefixedDirectoryWithoutASelector_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	address, root, opened := dotPrefixedRealm(c)

	output, err := hostApplyEverywhere(c, address, "topics:\n  events:\n    schema: .hidden\n    min_active_partitions: 1\n")

	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Not(qt.Contains), "absence was not established")
	c.Assert(output, qt.Contains, "Schema apply completed successfully")
	c.Assert(heldEntries(c, opened, path.Join(root, ".hidden")), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{"events": Ydb_Scheme.Entry_TOPIC})
}

// An object a directory whose name starts with a dot holds stays where it is
// when a run with no schema selector declares nothing there, as the host's
// embedded engine left it: the read does not describe it, so no drop is
// planned. The cost, the engine's too, is that a run declaring an object
// there that exists plans its creation again and fails; the provider refuses
// that unit before it sends it, and the directory keeps what it held.
func TestHostLeavesWhatADotPrefixedDirectoryHoldsAlone_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	address, root, opened := dotPrefixedRealm(c)
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE TOPIC `"+path.Join(root, ".hidden", "kept")+"`"), qt.IsNil)
	const declared = "topics:\n  events:\n    schema: .hidden\n    min_active_partitions: 1\n"

	created, createErr := hostApplyEverywhere(c, address, declared)
	again, againErr := hostApplyEverywhere(c, address, declared)

	c.Assert(createErr, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(created, qt.Not(qt.Contains), "kept")
	c.Assert(againErr, qt.IsNotNil, qt.Commentf("%s", again))
	c.Assert(again, qt.Contains, "provider execution: NOT_STARTED (0 units confirmed applied)")
	c.Assert(heldEntries(c, opened, path.Join(root, ".hidden")), qt.DeepEquals,
		map[string]Ydb_Scheme.Entry_Type{"events": Ydb_Scheme.Entry_TOPIC, "kept": Ydb_Scheme.Entry_TOPIC})
}

// A Go export of the whole database writes no `ptah:schema:notdescribed`
// entry for a native kind when the database holds the dev realm directory,
// and nothing a realm holds, as the host's embedded engine at 74de0b65b,
// which left the directory out of its read as no part of the schema. The read
// claims the directory as holding nothing; claimed unknown, it made the export
// name every native kind as not described, and a topic or a node left out of
// the export was then never dropped.
func TestHostIntrospectOfTheDatabaseClaimsTheDevRealms_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := inspectionFixture(c)
	realm := path.Join(database, "ptah_dev", directory)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		removeTree(ctx, opened, realm)
	})
	createSchemaObject(c, opened, "CREATE TOPIC `"+path.Join(realm, "events")+"`", "DROP TOPIC IF EXISTS `"+path.Join(realm, "events")+"`")
	out := c.TempDir()

	hostExport(c, "introspect", "--out", out, "--single-file")

	written, err := os.ReadFile(filepath.Join(out, "schema.go"))
	c.Assert(err, qt.IsNil)
	source := string(written)
	c.Assert(source, qt.Not(qt.Matches), `(?s).*//ptah:schema:notdescribed kind="(coordination_node|external_data_source|external_table|replication|secret|topic|transfer)".*`)
	c.Assert(source, qt.Not(qt.Contains), directory)
}

// dotPrefixedRealm is a dev realm of its own whose root holds the directory
// .hidden, and the connection that made it. The cleanup removes the
// directory, what it holds and the run's lock node, which a session on a
// realm keeps at the realm's root.
func dotPrefixedRealm(c *qt.C) (address, root string, opened *connection.Connection) {
	c.Helper()
	opened, _, _ = inspectionFixture(c)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	address, root = realmFixture(c, opened, base)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		removeTree(ctx, opened, path.Join(root, ".hidden"))
		_ = opened.Driver.Coordination().DropNode(ctx, path.Join(root, locking.NodeName))
	})
	c.Assert(opened.Driver.Scheme().MakeDirectory(c.Context(), path.Join(root, ".hidden")), qt.IsNil)
	return address, root, opened
}

// A run with no schema selector reads the realm's root, which holds the
// coordination node the run locks on. The provider reserves that name for
// every native kind, and a reservation is no streaming query. A run that
// changes a topic, an input a streaming query could read, asks which queries
// read it, and the reserved name is not one: the change plans and applies
// with the node at the root, and the next run is synced. The node is the
// run's own, taken before the plan, so every run meets it.
func TestHostChangesATopicWithoutASelectorBesideTheLockNode_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, _, _ := lockFixture(c)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	address, root := realmFixture(c, opened, base)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP TOPIC IF EXISTS `"+path.Join(root, "events")+"`"), qt.IsNil)
		_ = opened.Driver.Coordination().DropNode(ctx, path.Join(root, locking.NodeName))
	})
	for _, run := range []struct{ name, source, want string }{
		{"the creation", "topics:\n  events:\n    retention_period: PT36H\n", `(?s).*Schema apply completed successfully.*`},
		{"the change", "topics:\n  events:\n    retention_period: P2D\n", `(?s).*Schema apply completed successfully.*`},
		{"a run after it", "topics:\n  events:\n    retention_period: P2D\n", `(?s).*Schema is synced, no changes to be made.*`},
	} {
		output, err := hostApplyEverywhere(c, address, run.source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", run.name, output))
		c.Assert(output, qt.Matches, run.want, qt.Commentf("%s", run.name))
	}
	c.Assert(heldEntries(c, opened, root), qt.DeepEquals, map[string]Ydb_Scheme.Entry_Type{
		"events": Ydb_Scheme.Entry_TOPIC, locking.NodeName: Ydb_Scheme.Entry_COORDINATION_NODE,
	})
}
