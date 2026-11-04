package tablecreate

import (
	"fmt"
	"slices"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
)

// partitioningChange is what one AlterTable call does to a table's settings:
// its automatic partitioning, its read replicas and its key bloom filter. The
// host's embedded engine changes them together, in one ALTER TABLE ... SET.
type partitioningChange struct {
	// set holds the partitioning settings the call changes and nothing else: a
	// field it does not name stays as the table holds it. It is nil when the
	// partitioning does not change.
	set *Ydb_Table.PartitioningSettings
	// held is the whole partitioning the table held when the change was planned.
	held *Ydb_Table.PartitioningSettings
	// bloom is the key bloom filter the call sets, and unspecified when it
	// leaves the filter as the table holds it. heldBloom is the filter the
	// table held.
	bloom, heldBloom Ydb.FeatureFlag_Status
	// replicas are the read replicas the call sets, and nil when it leaves them
	// as the table holds them. heldReplicas are the replicas the table held, nil
	// for none.
	replicas, heldReplicas *Ydb_Table.ReadReplicasSettings
}

func (c partitioningChange) empty() bool {
	return c.set == nil && c.bloom == Ydb.FeatureFlag_STATUS_UNSPECIFIED && c.replicas == nil
}

// partitioningInput is what the settings rule reads. managed is true when
// the declaration states table properties for this target; a declaration with
// no table properties leaves the settings alone.
type partitioningInput struct {
	managed bool
	desired *Ydb_Table.PartitioningSettings
	held    *Ydb_Table.PartitioningSettings
	// layout is the starting layout the declaration states, which YDB takes only
	// when it creates a table.
	layout layout
	// desiredBloom and heldBloom are the key bloom filter the declaration
	// states and the one the table holds.
	desiredBloom, heldBloom Ydb.FeatureFlag_Status
	// desiredReplicas and heldReplicas are the read replicas the declaration
	// states, nil where it states none, and the ones the table holds.
	desiredReplicas, heldReplicas *Ydb_Table.ReadReplicasSettings
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

// defaultPartitionSizeMB is the partition size YDB gives a table when it
// turns partitioning by size on without a size: measured on 25.1.4.7 and
// 26.2.1.14, `SET (AUTO_PARTITIONING_BY_SIZE = ENABLED)` on a table that did
// not split by size leaves 2048 MB.
const defaultPartitionSizeMB = 2048

// resolvedPartitioning returns every setting a table holding held keeps once
// the settings want states replace its own, without the partition columns: the
// stated value of each setting, else the held one. A size is named only where
// one is stated or held; a table that turns partitioning by size on takes
// defaultPartitionSizeMB otherwise.
func resolvedPartitioning(want, held *Ydb_Table.PartitioningSettings) *Ydb_Table.PartitioningSettings {
	result := ApplyPartitioning(held, want)
	result.PartitionBy = nil
	return result
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

// withSizeSplitting returns want with partitioning by size turned on where it
// declares a partition size and no switch, and want itself otherwise. YDB
// turns splitting by size on when a size is set: measured by the host's
// embedded engine on 25.1.4.7 and 26.2.1.14, `SET
// (AUTO_PARTITIONING_PARTITION_SIZE_MB = 100)` on a table or an index that
// does not split by size leaves it splitting by size at 100 MB. That engine
// reads such a declaration the same way, so the change names the switch and,
// because turning it on resets the settings the call leaves out, every other
// setting too.
func withSizeSplitting(want *Ydb_Table.PartitioningSettings) *Ydb_Table.PartitioningSettings {
	if want.GetPartitionSizeMb() == 0 || want.GetPartitioningBySize() != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		return want
	}
	result := proto.CloneOf(want)
	result.PartitioningBySize = Ydb.FeatureFlag_ENABLED
	return result
}

// partitioningRefusal says why a table holding held cannot take the settings
// set, or returns the empty string. The server turns partitioning by size on
// when a partition size is set, so a size beside a switch the declaration
// turns off is refused rather than left to contradict the plan, and a minimum
// above a nonzero maximum is refused by name.
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
// partitioning settings, the read replicas and the key bloom filter the
// declaration states, or the reason it cannot be made. An empty change means
// the table already satisfies the declaration.
//
// Only AUTO_PARTITIONING_BY_SIZE, AUTO_PARTITIONING_BY_LOAD,
// AUTO_PARTITIONING_PARTITION_SIZE_MB, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT
// and AUTO_PARTITIONING_MAX_PARTITIONS_COUNT change after creation. The
// partition columns are set when the table is created, so a declaration that
// names other columns is refused rather than ignored. A starting layout is set
// when the table is created too: a declaration of one is refused when the
// minimum partition count it implies is not the one the table holds.
//
// The read replicas and the key bloom filter change in the same call. A
// declaration that leaves either out keeps what the table holds.
func planPartitioning(in partitioningInput) (partitioningChange, string) {
	if !in.managed {
		return partitioningChange{}, ""
	}
	if reason := layoutRefusal(in.layout, in.desired, in.held); reason != "" {
		return partitioningChange{}, reason
	}
	change := partitioningChange{bloom: bloomDelta(in.desiredBloom, in.heldBloom), replicas: replicasDelta(in.desiredReplicas, in.heldReplicas)}
	if change.bloom != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		change.heldBloom = in.heldBloom
	}
	if change.replicas != nil {
		change.heldReplicas = heldReplicas(in.heldReplicas)
	}
	if in.desired == nil {
		return change, ""
	}
	if columns := in.desired.GetPartitionBy(); len(columns) != 0 && !slices.Equal(columns, in.held.GetPartitionBy()) {
		return partitioningChange{}, fmt.Sprintf("the partition columns %q differ from the %q the table holds, and YDB sets them only when the table is created", columns, in.held.GetPartitionBy())
	}
	desired := withSizeSplitting(in.desired)
	delta := partitioningDelta(desired, in.held)
	if delta == nil {
		return change, ""
	}
	if reason := partitioningRefusal(in.held, delta); reason != "" {
		return partitioningChange{}, reason
	}
	if delta.GetPartitioningBySize() == Ydb.FeatureFlag_ENABLED {
		// Switching the size partitioning on resets the size and the minimum
		// count the call does not name, so the call names every setting the
		// table keeps: the stated ones and the held value of the others, as
		// the host's embedded engine writes them.
		delta = resolvedPartitioning(desired, in.held)
	}
	held := proto.CloneOf(in.held)
	if held == nil {
		held = &Ydb_Table.PartitioningSettings{}
	}
	change.set, change.held = delta, held
	return change, ""
}

// bloomEnabled reports whether a table keeps a bloom filter of its keys. A new
// table describes its filter as unspecified, and keeps none: measured on
// 25.1.4.7 and 26.2.1.14 by the host's embedded engine, unspecified and
// DISABLED both mean no filter.
func bloomEnabled(status Ydb.FeatureFlag_Status) bool { return status == Ydb.FeatureFlag_ENABLED }

// SameKeyBloomFilter reports whether two descriptions of a key bloom filter
// keep the same filter: an unspecified one is DISABLED.
func SameKeyBloomFilter(a, b Ydb.FeatureFlag_Status) bool { return bloomEnabled(a) == bloomEnabled(b) }

// bloomDelta returns the key bloom filter a table holding held has to be
// given to satisfy want, or unspecified when it already does or want states
// nothing.
func bloomDelta(want, held Ydb.FeatureFlag_Status) Ydb.FeatureFlag_Status {
	if want == Ydb.FeatureFlag_STATUS_UNSPECIFIED || bloomEnabled(want) == bloomEnabled(held) {
		return Ydb.FeatureFlag_STATUS_UNSPECIFIED
	}
	return want
}

// replicaCount reads read replicas as their mode and count. Nil, and settings
// that name no mode, are none.
func replicaCount(replicas *Ydb_Table.ReadReplicasSettings) (perAZ bool, count uint64) {
	switch settings := replicas.GetSettings().(type) {
	case *Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount:
		return true, settings.PerAzReadReplicasCount
	case *Ydb_Table.ReadReplicasSettings_AnyAzReadReplicasCount:
		return false, settings.AnyAzReadReplicasCount
	}
	return false, 0
}

// SameReplicas reports whether two read replica settings keep the same
// replicas. A count of zero is none in either mode: `PER_AZ:0` is how a
// declaration removes the replicas a table holds, since YDB has no RESET for
// them, and a table read back after it holds none.
func SameReplicas(a, b *Ydb_Table.ReadReplicasSettings) bool {
	aPerAZ, aCount := replicaCount(a)
	bPerAZ, bCount := replicaCount(b)
	return aCount == bCount && (aCount == 0 || aPerAZ == bPerAZ)
}

// replicasDelta returns the read replicas a table holding held has to be
// given to satisfy want, or nil when it already does or want states nothing.
// Replicas declared as none, on a table that holds some, are written as
// `PER_AZ:0`.
func replicasDelta(want, held *Ydb_Table.ReadReplicasSettings) *Ydb_Table.ReadReplicasSettings {
	if want.GetSettings() == nil || SameReplicas(want, held) {
		return nil
	}
	if _, count := replicaCount(want); count == 0 {
		return &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount{}}
	}
	return proto.CloneOf(want)
}

// heldReplicas returns the read replicas a table holds, never nil: settings
// that name no mode stand for none.
func heldReplicas(held *Ydb_Table.ReadReplicasSettings) *Ydb_Table.ReadReplicasSettings {
	if held == nil {
		return &Ydb_Table.ReadReplicasSettings{}
	}
	return proto.CloneOf(held)
}
