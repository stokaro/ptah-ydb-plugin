//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A size declared for a table that does not split by size turns splitting by
// size on, as YDB and the host's embedded engine do: the change names the
// switch, the size and the minimum the table holds, so the minimum survives,
// and nothing is left to plan after it.
func TestPartitionSizeTurnsSplittingOnSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, "", " WITH (AUTO_PARTITIONING_BY_SIZE = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 6)")
	declared := partitioningDeclaration(`{"partitionSizeMb":"100"}`)
	found := fixture.compare(c, declared)
	c.Assert(found.Changes, qt.HasLen, 1)
	fixture.apply(c, "partition-size", declared, found.Changes)
	held := fixture.heldPartitioning(c)
	c.Assert([]any{held["partitioningBySize"], held["partitionSizeMb"], held["minPartitionsCount"]}, qt.DeepEquals, []any{"ENABLED", "100", "6"})
	c.Assert(fixture.compare(c, declared).Changes, qt.HasLen, 0)
}

// TestGlobalIndexSettingOneResetsAnotherSubprocess is the host's
// TestYDBGlobalIndexes_SettingOneResetsAnother. It changes an index's settings
// where writing only the ones that differ would leave others reset: setting
// AUTO_PARTITIONING_BY_LOAD resets the minimum partition count, and setting
// AUTO_PARTITIONING_BY_SIZE resets it and the size. Each change converges in
// one apply, which it does only because the statement names every setting,
// the held value of one the declaration leaves out included.
func TestGlobalIndexSettingOneResetsAnotherSubprocess(t *testing.T) {
	for _, step := range []struct {
		name          string
		before, after string
		want          string
	}{
		{
			name:   "splitting by load turned on beside a minimum",
			before: `"partitioningSettings":{"minPartitionsCount":"5"}`,
			after:  `"partitioningSettings":{"partitioningByLoad":"ENABLED","minPartitionsCount":"5"}`,
			want:   `{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"5"}}`,
		},
		{
			name:   "splitting by size turned back on beside a minimum",
			before: `"partitioningSettings":{"partitioningBySize":"DISABLED","minPartitionsCount":"6"}`,
			after:  `"partitioningSettings":{"partitionSizeMb":"100","minPartitionsCount":"6"}`,
			want:   `{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"100","partitioningByLoad":"DISABLED","minPartitionsCount":"6"}}`,
		},
		{
			name:   "splitting by load turned on beside a held minimum",
			before: `"partitioningSettings":{"minPartitionsCount":"5"}`,
			after:  `"partitioningSettings":{"partitioningByLoad":"ENABLED"}`,
			want:   `{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"5"}}`,
		},
	} {
		t.Run(step.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := newFacetFixture(c, ", INDEX by_payload GLOBAL SYNC ON (payload)", "")
			fixture.applyIndexSettings(c, "settings-before", declaredIndexes(indexWithSettings("globalIndex", "", step.before)))
			declared := declaredIndexes(indexWithSettings("globalIndex", "", step.after))
			fixture.applyIndexSettings(c, "settings-after", declared)
			c.Assert(fixture.compareIndexes(c, declared).Changes, qt.HasLen, 0)
			c.Assert(fixture.heldIndexSettings(c), qt.JSONEquals, decodedJSON(c, step.want))
		})
	}
}
