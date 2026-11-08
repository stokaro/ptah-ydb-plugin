//go:build integration

package integration_test

import (
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// perTableFiles reads the Go files a per-table export wrote to out, by name.
func perTableFiles(c *qt.C, out string) map[string]string {
	c.Helper()
	entries, err := os.ReadDir(out)
	c.Assert(err, qt.IsNil)
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(out, entry.Name()))
		c.Assert(err, qt.IsNil)
		files[entry.Name()] = string(data)
	}
	return files
}

// notDescribedLines are the ptah:schema:notdescribed lines of a Go file.
func notDescribedLines(source string) []string {
	return slices.DeleteFunc(strings.Split(source, "\n"), func(line string) bool {
		return !strings.HasPrefix(line, "//ptah:schema:notdescribed ")
	})
}

// A per-table Go export writes each table in a file of its own and the other
// objects in schema_objects.go. Each file is part of a source, so each states
// the namespaces the read did not inspect as not described, as every file of
// the host's embedded engine's per-table export did: with resource pools off,
// the resource pools and their classifiers. With resource pools on, the read
// inspects both, and no file states them. Either way the files read together
// describe the directory: applied back, they plan nothing. Ported from the host's
// TestGoExportPartialFilesKeepUnknownNamespaces. The built-in engine at host
// 74de0b65b refused the read with resource pools off (`resource pools
// namespace is not fully described: resource pool support is unavailable`),
// measured on 26.2.1.14.
func TestHostPerTableGoExportKeepsUnknownNamespacesInEveryFile_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		pools bool
		want  []string
	}{
		{name: "resource pools off", pools: false, want: []string{
			`//ptah:schema:notdescribed kind="resource_pool" reason="not-inspected" provenance="observed"`,
			`//ptah:schema:notdescribed kind="resource_pool_classifier" reason="not-inspected" provenance="observed"`,
		}},
		{name: "resource pools on", pools: true, want: make([]string, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, directory, database := lockFixture(c)
			monitoredAddress(c)
			newClusterFlags(c, opened).set(c, map[string]bool{"EnableResourcePools": test.pools})
			for _, name := range []string{"first", "second"} {
				absolute := path.Join(database, directory, name)
				createSchemaObject(c, opened, "CREATE TABLE `"+absolute+"` (id Uint64 NOT NULL, PRIMARY KEY (id))", "DROP TABLE IF EXISTS `"+absolute+"`")
			}
			createNode(c, opened, path.Join(database, directory, "worker"))
			out := c.TempDir()

			hostExport(c, "introspect", "--schemas", directory, "--out", out, "--per-table")
			files := perTableFiles(c, out)
			planned, err := hostCommand(c, "schema", "apply", "--dry-run", "--db-url", os.Getenv("PTAH_YDB_TEST_URL"), "--schemas", directory, "--root-dir", out)

			c.Assert(slices.Sorted(maps.Keys(files)), qt.DeepEquals, []string{directory + "_first.go", directory + "_second.go", "schema_objects.go"})
			c.Assert(files["schema_objects.go"], qt.Contains, `//ptah:schema:coordinationnode name="worker" schema="`+directory+`"`)
			for name, source := range files {
				c.Assert(notDescribedLines(source), qt.DeepEquals, test.want, qt.Commentf("%s:\n%s", name, source))
			}
			c.Assert(err, qt.IsNil, qt.Commentf("%s", planned))
			c.Assert(planned, qt.Contains, "Schema is synced, no changes to be made.")
		})
	}
}
