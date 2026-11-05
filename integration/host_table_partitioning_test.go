//go:build integration

package integration_test

import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBTablePartitioning_RoundTrip: the host creates
// a table with every splitting setting, read replicas and a key bloom filter,
// reads them back and plans nothing after. It then turns splitting by load
// on, which alone would reset the minimum, and the table keeps the size, the
// minimum, the maximum, the read replicas and the bloom filter the
// declaration leaves out. Declared away through PER_AZ:0 and DISABLED, since
// neither can be reset, the replicas and the filter go. With splitting by
// size off YDB reports no partition size, as the host's test reads it on both
// lines; a declaration that turns it on again names the size, and one that
// names no setting plans nothing, the maximum YDB cannot remove included.
func TestHostChangesTablePartitioning_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	absolute := path.Join(database, directory, "items")
	emptyAtCleanup(c, opened, path.Join(database, directory))
	tuned := ` auto_partitioning_partition_size_mb="100" auto_partitioning_min_partitions_count="6" auto_partitioning_max_partitions_count="20"` +
		` read_replicas_settings="PER_AZ:1" key_bloom_filter="ENABLED"`
	applyUntilSynced(c, directory, "go", replaceDirectory(partitionedItems(tuned), directory))
	c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, storedTable{Families: []string{"default compression=off"},
		Partitioning: "by_size=ENABLED size=100 by_load=DISABLED min=6 max=20", ReadReplicas: "PER_AZ:1", KeyBloomFilter: "ENABLED"})

	for _, step := range []struct {
		name, attributes string
		want             storedTable
	}{
		{name: "by load turned on", attributes: ` auto_partitioning_by_load="ENABLED"`,
			want: storedTable{Partitioning: "by_size=ENABLED size=100 by_load=ENABLED min=6 max=20", ReadReplicas: "PER_AZ:1", KeyBloomFilter: "ENABLED"}},
		{name: "replicas and filter declared away", attributes: ` read_replicas_settings="PER_AZ:0" key_bloom_filter="DISABLED"`,
			want: storedTable{Partitioning: "by_size=ENABLED size=100 by_load=ENABLED min=6 max=20", KeyBloomFilter: "DISABLED"}},
		{name: "by size turned off", attributes: ` auto_partitioning_by_size="DISABLED" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="6" auto_partitioning_max_partitions_count="20"`,
			want: storedTable{Partitioning: "by_size=DISABLED size=0 by_load=ENABLED min=6 max=20", KeyBloomFilter: "DISABLED"}},
		{name: "by size turned on again", attributes: ` auto_partitioning_partition_size_mb="100" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="6" auto_partitioning_max_partitions_count="20"`,
			want: storedTable{Partitioning: "by_size=ENABLED size=100 by_load=ENABLED min=6 max=20", KeyBloomFilter: "DISABLED"}},
	} {
		source := replaceDirectory(partitionedItems(step.attributes), directory)
		output, err := hostApplySource(c, directory, "go", source)
		c.Assert(err, qt.IsNil, qt.Commentf("%s: %s", step.name, output))
		c.Assert(output, qt.Matches, `(?s).*row-table-alter-operation.*Schema apply completed successfully.*`, qt.Commentf("%s: %s", step.name, output))
		c.Assert(hostPlanned(c, directory, "go", source), qt.HasLen, 0, qt.Commentf("%s", step.name))
		step.want.Families = []string{"default compression=off"}
		c.Assert(heldStorage(c, opened, absolute), qt.DeepEquals, step.want, qt.Commentf("%s", step.name))
	}
	c.Assert(hostPlanned(c, directory, "go", replaceDirectory(partitionedItems(""), directory)), qt.HasLen, 0)
}
