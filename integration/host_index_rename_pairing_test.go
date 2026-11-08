//go:build integration

package integration_test

import (
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

func indexPairingSource(indexes string) string {
	return "tables:\n  docs:\n    columns:\n" +
		"      id: {type: Uint64, primary: true, not_null: true}\n" +
		"      a: {type: Utf8}\n      b: {type: Utf8}\n" +
		"    indexes:\n" + indexes
}

// When two identical global indexes are renamed at once, the host pairs the
// names in sorted order even when the source files list them in reverse order.
// This ports TestIndexes_YDBPairsEqualRenamesInNameOrder against the indexes
// YDB holds after applying the first source.
func TestHostPairsEquivalentIndexRenamesInNameOrder_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	realm := newCLIRealm(c)
	files := writeFiles(c, c.TempDir(), map[string]string{
		"from.yaml": indexPairingSource("      old_2: {fields: [a]}\n      old_1: {fields: [a]}\n"),
		"to.yaml":   indexPairingSource("      new_2: {fields: [a]}\n      new_1: {fields: [a]}\n"),
	})
	seed, err := hostCommand(c, "schema", "apply", "--db-url", realm.address, "--schema-file", filepath.Join(files, "from.yaml"),
		"--auto-approve", "--tx-mode", "none")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", seed))
	out, err := hostCommand(c, "schema", "diff", "--from", realm.address,
		"--to", filepath.Join(files, "to.yaml"), "--dev-url", realm.address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	first := "RENAME INDEX `old_1` TO `new_1`"
	second := "RENAME INDEX `old_2` TO `new_2`"
	c.Assert(out, qt.Contains, first)
	c.Assert(out, qt.Contains, second)
	c.Assert(strings.Index(out, first) < strings.Index(out, second), qt.IsTrue, qt.Commentf("%s", out))
	c.Assert(out, qt.Not(qt.Contains), "DROP INDEX")
	c.Assert(out, qt.Not(qt.Contains), "ADD INDEX")
}

// Different kinds or key columns make a renamed index a removal and an
// addition. The host does not pair either change as an in-place rename.
// This ports the kind and column rows of TestIndexes_RenameNotPaired.
func TestHostDoesNotPairDifferentIndexRenames_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	realm := newCLIRealm(c)
	for _, test := range []struct {
		name, desired string
	}{
		{"another kind", "{fields: [a], type: async}"},
		{"another column", "{fields: [b]}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			files := writeFiles(c, c.TempDir(), map[string]string{
				"from.yaml": indexPairingSource("      old: {fields: [a]}\n"),
				"to.yaml":   indexPairingSource("      new: " + test.desired + "\n"),
			})
			out, err := hostCommand(c, "schema", "diff", "--from", filepath.Join(files, "from.yaml"),
				"--to", filepath.Join(files, "to.yaml"), "--dev-url", realm.address)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
			c.Assert(out, qt.Contains, "DROP INDEX `old`")
			c.Assert(out, qt.Contains, "ADD INDEX `new`")
			c.Assert(out, qt.Not(qt.Contains), "RENAME INDEX")
		})
	}
}

func twoIndexTables(firstIndex, secondIndex string) string {
	columns := "    columns:\n      id: {type: Uint64, primary: true, not_null: true}\n      a: {type: Utf8}\n"
	return "tables:\n  first:\n" + columns + firstIndex + "  second:\n" + columns + secondIndex
}

// An index on one table and an equal index on another are independent changes
// even when their names suggest a rename. This ports the cross-table row of
// TestIndexes_RenameNotPaired.
func TestHostDoesNotPairIndexesOfDifferentTables_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	requireHostNegotiates(c, provider.IndexRenamesFeature)
	realm := newCLIRealm(c)
	files := writeFiles(c, c.TempDir(), map[string]string{
		"from.yaml": twoIndexTables("    indexes:\n      old: {fields: [a]}\n", ""),
		"to.yaml":   twoIndexTables("", "    indexes:\n      new: {fields: [a]}\n"),
	})
	out, err := hostCommand(c, "schema", "diff", "--from", filepath.Join(files, "from.yaml"),
		"--to", filepath.Join(files, "to.yaml"), "--dev-url", realm.address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", out))
	c.Assert(out, qt.Contains, "ALTER TABLE `first` DROP INDEX `old`")
	c.Assert(out, qt.Contains, "ALTER TABLE `second` ADD INDEX `new`")
	c.Assert(out, qt.Not(qt.Contains), "RENAME INDEX")
}
