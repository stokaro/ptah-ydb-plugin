package schema

import (
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// IndexSettingsProperties identifies the automatic partitioning and the read
// replicas of one global sync, async or unique index, owned by the index: kind
// "index", the table as the only path component and the index's name.
//
// A read reports every setting the index holds, defaults included. A source
// states the settings it declares, and a setting it leaves out keeps what the
// index holds: a new index takes it from YDB, and a plan never changes it. An
// index does not take its table's settings.
const IndexSettingsProperties = modelNamespace + "index-properties"

// NewIndexSettings returns the settings YDB gives a new global index whatever
// its table holds, measured on 25.1.4.7 and 26.2.1.14: automatic partitioning
// by size at 2048 MB, none by load, at least one partition, no maximum and no
// read replicas.
func NewIndexSettings() *native.IndexSettingsObservation {
	return &native.IndexSettingsObservation{PartitioningSettings: &Ydb_Table.PartitioningSettings{
		PartitioningBySize: Ydb.FeatureFlag_ENABLED, PartitionSizeMb: 2048, PartitioningByLoad: Ydb.FeatureFlag_DISABLED, MinPartitionsCount: 1}}
}

// CaptureIndexSettings records the settings an index holds as its observed
// facet. owner is the index, and settings are the ones the server describes on
// it: partition columns are dropped, since YDB partitions an index on its own
// key, and read replicas with a count of zero are none.
func CaptureIndexSettings(owner *protocol.Reference, settings *native.GlobalIndexSettings) (*protocol.Envelope, error) {
	observed := &native.IndexSettingsObservation{}
	if partitioning := settings.GetPartitioningSettings(); partitioning != nil {
		observed.PartitioningSettings = proto.CloneOf(partitioning)
		observed.PartitioningSettings.PartitionBy = nil
	}
	if replicas := settings.GetReadReplicasSettings(); replicaCount(replicas) != 0 {
		observed.ReadReplicasSettings = proto.CloneOf(replicas)
	}
	return capture(IndexSettingsProperties, owner, observed)
}

// CaptureIndexSettingsIntent records the settings a declaration states for
// an index as its desired facet. owner is the index: kind "index", its table
// as the only path component and its name.
func CaptureIndexSettingsIntent(owner *protocol.Reference, intent *native.IndexSettingsIntent) (*protocol.Envelope, error) {
	return convertedEnvelope(&protocol.Envelope{ProviderId: ProviderID, Kind: IndexSettingsProperties, Owner: proto.CloneOf(owner)},
		NativeDesiredModel(IndexSettingsProperties), intent, NormalizeDesired)
}

func replicaCount(replicas *Ydb_Table.ReadReplicasSettings) uint64 {
	return replicas.GetPerAzReadReplicasCount() + replicas.GetAnyAzReadReplicasCount()
}

// validateIndexSettings refuses a setting an index does not take: partition
// columns, which YDB keys on the index's own key, a count of zero, which no
// statement sets, a minimum above a nonzero maximum, and read replicas that
// name no mode. A declaration states no read replicas with a count of zero;
// see validateObservedIndexSettings for a read.
func validateIndexSettings(partitioning *Ydb_Table.PartitioningSettings, replicas *Ydb_Table.ReadReplicasSettings) error {
	if partitioning != nil {
		if len(partitioning.GetPartitionBy()) != 0 {
			return fmt.Errorf("YDB index settings name partition columns, and YDB partitions an index on its own key")
		}
		if partitioning.GetMaxPartitionsCount() != 0 && partitioning.GetMinPartitionsCount() > partitioning.GetMaxPartitionsCount() {
			return fmt.Errorf("YDB minimum partitions exceed maximum")
		}
		if proto.Size(partitioning) == 0 {
			return fmt.Errorf("YDB index partitioning states no setting")
		}
	}
	if replicas != nil {
		if replicas.GetSettings() == nil {
			return fmt.Errorf("YDB index read replicas name no mode")
		}
	}
	return nil
}

// validateObservedIndexSettings is [validateIndexSettings] for a read, which
// reports no read replicas as none and refuses a count of zero.
func validateObservedIndexSettings(partitioning *Ydb_Table.PartitioningSettings, replicas *Ydb_Table.ReadReplicasSettings) error {
	if err := validateIndexSettings(partitioning, replicas); err != nil {
		return err
	}
	if replicas != nil && replicaCount(replicas) == 0 {
		return fmt.Errorf("observed YDB index read replicas report a count of zero, which a read reports as none")
	}
	return nil
}

// indexSettingsIntent writes an observation as the declaration that states
// what it holds: the settings that differ from [NewIndexSettings], as the
// host's embedded engine writes them. An index that holds the settings a new
// index takes states none, which is a declaration that keeps them.
func indexSettingsIntent(observed *native.IndexSettingsObservation) *native.IndexSettingsIntent {
	defaults := NewIndexSettings().GetPartitioningSettings()
	held := observed.GetPartitioningSettings()
	partitioning := &Ydb_Table.PartitioningSettings{}
	if held.GetPartitioningBySize() != Ydb.FeatureFlag_STATUS_UNSPECIFIED && held.GetPartitioningBySize() != defaults.GetPartitioningBySize() {
		partitioning.PartitioningBySize = held.GetPartitioningBySize()
	}
	if held.GetPartitionSizeMb() != 0 && held.GetPartitionSizeMb() != defaults.GetPartitionSizeMb() {
		partitioning.PartitionSizeMb = held.GetPartitionSizeMb()
	}
	if held.GetPartitioningByLoad() != Ydb.FeatureFlag_STATUS_UNSPECIFIED && held.GetPartitioningByLoad() != defaults.GetPartitioningByLoad() {
		partitioning.PartitioningByLoad = held.GetPartitioningByLoad()
	}
	if held.GetMinPartitionsCount() > defaults.GetMinPartitionsCount() {
		partitioning.MinPartitionsCount = held.GetMinPartitionsCount()
	}
	partitioning.MaxPartitionsCount = held.GetMaxPartitionsCount()
	result := &native.IndexSettingsIntent{ReadReplicasSettings: proto.CloneOf(observed.GetReadReplicasSettings())}
	if proto.Size(partitioning) != 0 {
		result.PartitioningSettings = partitioning
	}
	return result
}

// indexSettingsObservation predicts what an index declared with intent holds
// once YDB builds it: the settings intent states over [NewIndexSettings]. A
// size stated without partitioning by size turns splitting by size on, as YDB
// does, and replicas stated as none are none.
func indexSettingsObservation(intent *native.IndexSettingsIntent) *native.IndexSettingsObservation {
	result := NewIndexSettings()
	held, stated := result.PartitioningSettings, intent.GetPartitioningSettings()
	if stated.GetPartitioningBySize() != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		held.PartitioningBySize = stated.GetPartitioningBySize()
	} else if stated.GetPartitionSizeMb() != 0 {
		held.PartitioningBySize = Ydb.FeatureFlag_ENABLED
	}
	if stated.GetPartitionSizeMb() != 0 {
		held.PartitionSizeMb = stated.GetPartitionSizeMb()
	}
	if stated.GetPartitioningByLoad() != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		held.PartitioningByLoad = stated.GetPartitioningByLoad()
	}
	if stated.GetMinPartitionsCount() != 0 {
		held.MinPartitionsCount = stated.GetMinPartitionsCount()
	}
	held.MaxPartitionsCount = stated.GetMaxPartitionsCount()
	if replicas := intent.GetReadReplicasSettings(); replicaCount(replicas) != 0 {
		result.ReadReplicasSettings = proto.CloneOf(replicas)
	}
	return result
}

// alteredIndexSettings predicts what an index holding held reads back with
// once a change gives it the settings declared states: each setting declared
// in place of the held one, as the change names every partitioning setting the
// index ends with. A size declared without partitioning by size turns it on,
// partitioning by size turned on without a size takes the 2048 MB YDB gives
// it, an index that ends without it reports no size, and replicas declared as
// none are none, as YDB does.
func alteredIndexSettings(held *native.IndexSettingsObservation, declared *native.IndexSettingsIntent) *native.IndexSettingsObservation {
	result := &native.IndexSettingsObservation{PartitioningSettings: proto.CloneOf(held.GetPartitioningSettings()), ReadReplicasSettings: proto.CloneOf(held.GetReadReplicasSettings())}
	if result.PartitioningSettings == nil {
		result.PartitioningSettings = &Ydb_Table.PartitioningSettings{}
	}
	partitioning, stated := result.PartitioningSettings, declared.GetPartitioningSettings()
	switch {
	case stated.GetPartitioningBySize() != Ydb.FeatureFlag_STATUS_UNSPECIFIED:
		partitioning.PartitioningBySize = stated.GetPartitioningBySize()
	case stated.GetPartitionSizeMb() != 0:
		partitioning.PartitioningBySize = Ydb.FeatureFlag_ENABLED
	}
	switch {
	case partitioning.GetPartitioningBySize() != Ydb.FeatureFlag_ENABLED:
		partitioning.PartitionSizeMb = 0
	case stated.GetPartitionSizeMb() != 0:
		partitioning.PartitionSizeMb = stated.GetPartitionSizeMb()
	case partitioning.GetPartitionSizeMb() == 0:
		partitioning.PartitionSizeMb = NewIndexSettings().GetPartitioningSettings().GetPartitionSizeMb()
	}
	if stated.GetPartitioningByLoad() != Ydb.FeatureFlag_STATUS_UNSPECIFIED {
		partitioning.PartitioningByLoad = stated.GetPartitioningByLoad()
	}
	if stated.GetMinPartitionsCount() != 0 {
		partitioning.MinPartitionsCount = stated.GetMinPartitionsCount()
	}
	if stated.GetMaxPartitionsCount() != 0 {
		partitioning.MaxPartitionsCount = stated.GetMaxPartitionsCount()
	}
	if replicas := declared.GetReadReplicasSettings(); replicas.GetSettings() != nil {
		result.ReadReplicasSettings = nil
		if replicaCount(replicas) != 0 {
			result.ReadReplicasSettings = proto.CloneOf(replicas)
		}
	}
	return result
}

// explicitIndexSettings writes what an index holds as a declaration that
// names every setting, so it resolves to them over whatever the index holds,
// as the host's embedded engine restores an index's settings: the partition
// size only where the index splits by size, and read replicas held as none
// declared as none. No declaration removes a maximum partition count, which
// YDB cannot remove, so an index that holds none declares none.
func explicitIndexSettings(held *native.IndexSettingsObservation) *native.IndexSettingsIntent {
	partitioning := held.GetPartitioningSettings()
	result := &native.IndexSettingsIntent{PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitioningBySize: partitioning.GetPartitioningBySize(),
		PartitioningByLoad: partitioning.GetPartitioningByLoad(), MinPartitionsCount: partitioning.GetMinPartitionsCount(), MaxPartitionsCount: partitioning.GetMaxPartitionsCount()}}
	if partitioning.GetPartitioningBySize() == Ydb.FeatureFlag_ENABLED {
		result.PartitioningSettings.PartitionSizeMb = partitioning.GetPartitionSizeMb()
	}
	result.ReadReplicasSettings = proto.CloneOf(held.GetReadReplicasSettings())
	if replicaCount(result.ReadReplicasSettings) == 0 {
		result.ReadReplicasSettings = &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount{}}
	}
	return result
}

// indexSettingsDefinition and indexSettingsDeclaration describe the two
// representations in the negotiated models, so a change of either rule
// changes the model's digest.
const (
	indexSettingsDefinition = "owned by a global sync, async or unique index: kind index, the table as the only path component; every partitioning setting the server describes on the index, " +
		"defaults included, without partition columns; read replicas with a count of zero are none and absent"
	indexSettingsDeclaration = "owned by a global sync, async or unique index; partitioning by size and by load, the partition size and the minimum and maximum partition counts, and the read replicas; " +
		"a setting left out keeps what the index holds; a size without partitioning by size turns it on; replicas with a count of zero remove them; " +
		"conversion from a read states the settings that differ from those YDB gives a new index"
)
