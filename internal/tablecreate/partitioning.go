package tablecreate

import (
	"fmt"
	"slices"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// partitioningChange is what one AlterTable call does to a table's automatic
// partitioning settings.
type partitioningChange struct {
	// set holds the settings the call changes and nothing else: a field it does
	// not name stays as the table holds it. It is nil when nothing changes.
	set *Ydb_Table.PartitioningSettings
	// held is the whole partitioning the table held when the change was planned.
	held *Ydb_Table.PartitioningSettings
}

func (c partitioningChange) empty() bool { return c.set == nil }

// partitioningInput is what the partitioning rule reads. managed is true when
// the declaration states table properties for this target; a declaration with
// no table properties leaves the partitioning alone.
type partitioningInput struct {
	managed bool
	desired *Ydb_Table.PartitioningSettings
	held    *Ydb_Table.PartitioningSettings
}

// partitioningDelta returns the settings of want that a table holding held does
// not already have, or nil. A setting the declaration leaves out (zero, or an
// unspecified switch) states nothing: the AlterTable call can neither reset it
// nor read it as a request, so it is never compared. The partition columns are
// not compared either, because only table creation sets them.
func partitioningDelta(want, held *Ydb_Table.PartitioningSettings) *Ydb_Table.PartitioningSettings {
	delta := &Ydb_Table.PartitioningSettings{}
	if want.GetPartitioningBySize() != Ydb.FeatureFlag_STATUS_UNSPECIFIED && want.GetPartitioningBySize() != held.GetPartitioningBySize() {
		delta.PartitioningBySize = want.PartitioningBySize
	}
	if want.GetPartitioningByLoad() != Ydb.FeatureFlag_STATUS_UNSPECIFIED && want.GetPartitioningByLoad() != held.GetPartitioningByLoad() {
		delta.PartitioningByLoad = want.PartitioningByLoad
	}
	if want.GetPartitionSizeMb() != 0 && want.GetPartitionSizeMb() != held.GetPartitionSizeMb() {
		delta.PartitionSizeMb = want.PartitionSizeMb
	}
	if want.GetMinPartitionsCount() != 0 && want.GetMinPartitionsCount() != held.GetMinPartitionsCount() {
		delta.MinPartitionsCount = want.MinPartitionsCount
	}
	if want.GetMaxPartitionsCount() != 0 && want.GetMaxPartitionsCount() != held.GetMaxPartitionsCount() {
		delta.MaxPartitionsCount = want.MaxPartitionsCount
	}
	if proto.Size(delta) == 0 {
		return nil
	}
	return delta
}

// statedPartitioning returns the settings of want that state a value, without
// the partition columns.
func statedPartitioning(want *Ydb_Table.PartitioningSettings) *Ydb_Table.PartitioningSettings {
	return &Ydb_Table.PartitioningSettings{PartitioningBySize: want.GetPartitioningBySize(), PartitioningByLoad: want.GetPartitioningByLoad(),
		PartitionSizeMb: want.GetPartitionSizeMb(), MinPartitionsCount: want.GetMinPartitionsCount(), MaxPartitionsCount: want.GetMaxPartitionsCount()}
}

// ApplyPartitioning returns the settings a table holds after an alteration
// that carries set: the fields set names replace the held ones, and every other
// field, the partition columns included, stays.
func ApplyPartitioning(held, set *Ydb_Table.PartitioningSettings) *Ydb_Table.PartitioningSettings {
	result := proto.CloneOf(held)
	if result == nil {
		result = &Ydb_Table.PartitioningSettings{}
	}
	if set.GetPartitioningBySize() != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		result.PartitioningBySize = set.PartitioningBySize
	}
	if set.GetPartitioningByLoad() != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		result.PartitioningByLoad = set.PartitioningByLoad
	}
	if set.GetPartitionSizeMb() != 0 {
		result.PartitionSizeMb = set.PartitionSizeMb
	}
	if set.GetMinPartitionsCount() != 0 {
		result.MinPartitionsCount = set.MinPartitionsCount
	}
	if set.GetMaxPartitionsCount() != 0 {
		result.MaxPartitionsCount = set.MaxPartitionsCount
	}
	return result
}

// partitioningRefusal says why a table holding held cannot take the settings
// set, or returns the empty string. The server turns partitioning by size on
// when a partition size is set, so a size beside a disabled switch is refused
// rather than left to contradict the plan, and a minimum above a nonzero
// maximum is refused by name.
func partitioningRefusal(held, set *Ydb_Table.PartitioningSettings) string {
	after := ApplyPartitioning(held, set)
	if set.GetPartitionSizeMb() != 0 && after.GetPartitioningBySize() == Ydb.FeatureFlag_DISABLED {
		return fmt.Sprintf("AUTO_PARTITIONING_PARTITION_SIZE_MB %d needs AUTO_PARTITIONING_BY_SIZE enabled, because YDB turns partitioning by size on when a size is set", set.GetPartitionSizeMb())
	}
	if after.MaxPartitionsCount != 0 && after.MinPartitionsCount > after.MaxPartitionsCount {
		return fmt.Sprintf("AUTO_PARTITIONING_MIN_PARTITIONS_COUNT %d exceeds AUTO_PARTITIONING_MAX_PARTITIONS_COUNT %d", after.MinPartitionsCount, after.MaxPartitionsCount)
	}
	return ""
}

// PartitioningHolds reports whether a table whose partitioning is got holds
// every setting set names. The server derives some settings from others: it
// reports no partition size while partitioning by size is off, and it resets
// the size and the minimum count when that switch is turned on. So a setting
// the request does not name is not compared.
func PartitioningHolds(got, set *Ydb_Table.PartitioningSettings) bool {
	return partitioningDelta(set, got) == nil
}

// planPartitioning returns the change that gives the table the automatic
// partitioning settings the declaration states, or the reason it cannot be
// made. An empty change means the table already satisfies the declaration.
//
// Only AUTO_PARTITIONING_BY_SIZE, AUTO_PARTITIONING_BY_LOAD,
// AUTO_PARTITIONING_PARTITION_SIZE_MB, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT
// and AUTO_PARTITIONING_MAX_PARTITIONS_COUNT change after creation. The
// partition columns are set when the table is created, so a declaration that
// names other columns is refused rather than ignored.
func planPartitioning(in partitioningInput) (partitioningChange, string) {
	if !in.managed || in.desired == nil {
		return partitioningChange{}, ""
	}
	if columns := in.desired.GetPartitionBy(); len(columns) != 0 && !slices.Equal(columns, in.held.GetPartitionBy()) {
		return partitioningChange{}, fmt.Sprintf("the partition columns %q differ from the %q the table holds, and YDB sets them only when the table is created", columns, in.held.GetPartitionBy())
	}
	delta := partitioningDelta(in.desired, in.held)
	if delta == nil {
		return partitioningChange{}, ""
	}
	if reason := partitioningRefusal(in.held, delta); reason != "" {
		return partitioningChange{}, reason
	}
	if delta.GetPartitioningBySize() == Ydb.FeatureFlag_ENABLED {
		// Switching the size partitioning on resets the size and the minimum
		// count the call does not name, so the call names every setting stated.
		delta = statedPartitioning(in.desired)
	}
	held := proto.CloneOf(in.held)
	if held == nil {
		held = &Ydb_Table.PartitioningSettings{}
	}
	return partitioningChange{set: delta, held: held}, ""
}
