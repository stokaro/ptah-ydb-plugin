//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/locking"
	"github.com/stokaro/ptah-ydb-plugin/internal/seeds"
)

// hostDataEntities declares a table and the rows of regions.yaml for it.
const hostDataEntities = "package entities\n\n//ptah:schema:data table=\"regions\" key=\"code\" file=\"regions.yaml\"\n" +
	"//ptah:schema:table name=\"regions\"\ntype Region struct {\n" +
	"\t//ptah:schema:field name=\"code\" type=\"Utf8\" primary=\"true\" not_null=\"true\"\n\tCode string\n" +
	"\t//ptah:schema:field name=\"name\" type=\"Utf8\" not_null=\"true\"\n\tName string\n" +
	"\t//ptah:schema:field name=\"rank\" type=\"Int64\"\n\tRank int64\n}\n"

// hostData is a dev realm of its own that the pinned host works in through
// this provider, so the table, the seed ledger and the schema lock the host
// takes are the test's.
type hostData struct {
	opened             *connection.Connection
	address, root      string
	entities, seedsDir string
	host, manifest     string
}

func newHostData(c *qt.C) *hostData {
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
	c.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{"regions", seeds.Table} {
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(root, table)+"`"), qt.IsNil)
		}
		// The host's schema lock leaves its node at the realm's root.
		_ = opened.Driver.Coordination().DropNode(cleanup, path.Join(root, locking.NodeName))
	})
	dir := c.TempDir()
	f := &hostData{opened: opened, address: address, root: root, entities: filepath.Join(dir, "entities"), seedsDir: filepath.Join(dir, "seeds"),
		host: host, manifest: manifest}
	for _, created := range []string{f.entities, f.seedsDir} {
		c.Assert(os.MkdirAll(created, 0o700), qt.IsNil)
	}
	c.Assert(os.WriteFile(filepath.Join(f.entities, "entities.go"), []byte(hostDataEntities), 0o600), qt.IsNil)
	return f
}

// run runs one host command line against the realm and returns what the
// host printed and how it exited.
func (f *hostData) run(c *qt.C, args ...string) (string, error) {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.host, append(args, "--provider-config", f.manifest, "--db-url", f.address)...)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := cmd.Run()
	return output.String(), err
}

func (f *hostData) declare(c *qt.C, rows string) {
	c.Helper()
	c.Assert(os.WriteFile(filepath.Join(f.entities, "regions.yaml"), []byte(rows), 0o600), qt.IsNil)
}

func (f *hostData) apply(c *qt.C) (string, error) {
	c.Helper()
	return f.run(c, "schema", "apply", "--auto-approve", "--tx-mode", "none", "--provider-engine", "ydb", "--root-dir", f.entities)
}

func (f *hostData) seed(c *qt.C, name, text string) {
	c.Helper()
	c.Assert(os.WriteFile(filepath.Join(f.seedsDir, name), []byte(text), 0o600), qt.IsNil)
}

func (f *hostData) runSeeds(c *qt.C, extra ...string) (string, error) {
	c.Helper()
	return f.run(c, append([]string{"seed", "--seeds-dir", f.seedsDir, "--env", "dev"}, extra...)...)
}

// stored reads the regions table back, code to name and rank, through a
// connection of its own.
func (f *hostData) stored(c *qt.C) map[string]string {
	c.Helper()
	rows, err := f.opened.DB.QueryContext(c.Context(), "SELECT code, name, rank FROM `"+path.Join(f.root, "regions")+"` ORDER BY code")
	c.Assert(err, qt.IsNil)
	defer func() { c.Check(rows.Close(), qt.IsNil) }()
	result := map[string]string{}
	for rows.Next() {
		var code, name string
		var rank *int64
		c.Assert(rows.Scan(&code, &name, &rank), qt.IsNil)
		result[code] = name + "/" + strconv.FormatInt(*rank, 10)
	}
	c.Assert(rows.Err(), qt.IsNil)
	return result
}

// The host writes declared rows through the provider: the provider types the
// declared and the live rows, the host plans the difference and the provider
// spells each write. The declared "1" and the stored 1 are one value once
// typed, so a converged table plans nothing.
func TestHostWritesDeclaredRowsThroughTheProvider_HappyPath(t *testing.T) {
	c := qt.New(t)
	f := newHostData(c)
	f.declare(c, "- code: NO\n  name: Norway\n  rank: 2\n- code: CZ\n  name: Czechia\n  rank: \"1\"\n")

	output, err := f.apply(c)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(f.stored(c), qt.DeepEquals, map[string]string{"CZ": "Czechia/1", "NO": "Norway/2"})

	output, err = f.apply(c)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*No changes planned.*`)

	f.declare(c, "- code: NO\n  name: Norge\n  rank: 2\n- code: SE\n  name: Sweden\n  rank: 3\n")
	output, err = f.apply(c)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(f.stored(c), qt.DeepEquals, map[string]string{"NO": "Norge/2", "SE": "Sweden/3"})
}

// The host runs seed files through the provider's ledger: a file runs once, a
// file that hits an existing key is refused and leaves nothing, and with
// --idempotent it is recorded without its rows.
func TestHostRunsSeedFilesThroughTheProvider_HappyPath(t *testing.T) {
	c := qt.New(t)
	f := newHostData(c)
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	c.Assert(execFixtureSchema(ctx, f.opened.DB, "CREATE TABLE `"+path.Join(f.root, "regions")+
		"` (code Utf8 NOT NULL, name Utf8 NOT NULL, rank Int64, PRIMARY KEY (code))"), qt.IsNil)
	f.seed(c, "001_regions.dev.sql", "INSERT INTO `regions` (code, name, rank) VALUES ('NO'u, 'Norway'u, 2);\n")

	output, err := f.runSeeds(c)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(f.stored(c), qt.DeepEquals, map[string]string{"NO": "Norway/2"})

	output, err = f.runSeeds(c)
	c.Assert(err, qt.IsNil, qt.Commentf("the ledger holds the file: %s", output))
	c.Assert(f.stored(c), qt.DeepEquals, map[string]string{"NO": "Norway/2"})

	f.seed(c, "002_more.dev.sql", "UPSERT INTO `regions` (code, name, rank) VALUES ('SE'u, 'Sweden'u, 3);\n"+
		"INSERT INTO `regions` (code, name, rank) VALUES ('NO'u, 'Norge'u, 2);\n")
	output, err = f.runSeeds(c)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "existing key")
	c.Assert(f.stored(c), qt.DeepEquals, map[string]string{"NO": "Norway/2"})

	output, err = f.runSeeds(c, "--idempotent")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(f.stored(c), qt.DeepEquals, map[string]string{"NO": "Norway/2"})
	output, err = f.runSeeds(c)
	c.Assert(err, qt.IsNil, qt.Commentf("the conflicting file is recorded: %s", output))
}
