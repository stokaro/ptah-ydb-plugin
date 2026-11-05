package yqlschema

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// tableFacets are the reply's facets of the YDB settings one table declares:
// the values a YAML or Go source of the table declares for the same settings,
// so a table means the same whichever format declares it. A row table's
// partitioning, read replicas, key bloom filter, starting layout, column
// families and TTL are one facet of the table, and each column a family other
// than the default one holds gets a facet that places it in that family. A
// column table's hash key, shard count and TTL are one column-storage facet of
// the table. A table that declares none of them gets none.
//
// A column table that declares a family is refused with the words the YAML
// and Go sources refuse it with, since the column-storage model holds no
// family, and so is a row table that declares both starting layouts, which the
// row-table model refuses where the facet is normalized.
func tableFacets(table Table) ([]*protocol.Envelope, error) {
	facets := table.Facets
	if facets.Empty() {
		return nil, nil
	}
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: table.Schema, Name: table.Name}
	if facets.ColumnStore != nil {
		storage, err := columnStorageIntent(facets)
		if err != nil {
			return nil, err
		}
		envelope, err := schema.CaptureColumnStorageIntent(owner, storage)
		if err != nil {
			return nil, err
		}
		return []*protocol.Envelope{envelope}, nil
	}
	intent, err := rowTableIntent(facets)
	if err != nil {
		return nil, err
	}
	envelope, err := schema.CaptureTableIntent(owner, intent)
	if err != nil {
		return nil, err
	}
	envelopes := []*protocol.Envelope{envelope}
	for _, family := range facets.Families {
		for _, column := range family.Columns {
			placed, err := schema.CaptureColumnIntent(&protocol.Reference{Engine: "ydb", Kind: "column", Schema: table.Schema, Path: []string{table.Name}, Name: column},
				&native.ColumnIntent{Family: family.Name})
			if err != nil {
				return nil, fmt.Errorf("column %s: %w", quoteName(column), err)
			}
			envelopes = append(envelopes, placed)
		}
	}
	return envelopes, nil
}

// rowTableIntent is a row table's settings as the row-table model holds them.
// A setting the declaration leaves out stays out, so the table keeps what it
// holds of it, as a YAML or Go source that leaves it out does.
func rowTableIntent(facets ydbdecl.TableFacets) (*native.RowTableIntent, error) {
	intent := &native.RowTableIntent{}
	if declared := facets.Partitioning; declared != nil {
		partitioning := &Ydb_Table.PartitioningSettings{PartitioningBySize: featureStatus(declared.BySize), PartitionSizeMb: declared.PartitionSizeMB,
			PartitioningByLoad: featureStatus(declared.ByLoad), MinPartitionsCount: declared.MinPartitions, MaxPartitionsCount: declared.MaxPartitions}
		if !proto.Equal(partitioning, &Ydb_Table.PartitioningSettings{}) {
			intent.PartitioningSettings = partitioning
		}
		intent.KeyBloomFilter = featureStatus(declared.KeyBloomFilter)
		if declared.ReadReplicas != "" {
			replicas, err := readReplicas(declared.ReadReplicas)
			if err != nil {
				return nil, err
			}
			intent.ReadReplicasSettings = replicas
		}
		intent.UniformPartitions = declared.UniformPartitions
		for _, point := range declared.PartitionAtKeys {
			intent.PartitionAtKeys = append(intent.PartitionAtKeys, &native.SplitPoint{Values: slices.Clone(point)})
		}
	}
	for _, family := range facets.Families {
		intent.ColumnFamilies = append(intent.ColumnFamilies, columnFamily(family))
	}
	if facets.TTL != nil {
		ttl, err := rowTTL(*facets.TTL)
		if err != nil {
			return nil, err
		}
		intent.TtlSettings = ttl
	}
	return intent, nil
}

// featureStatus writes a declared switch as YDB's status: ENABLED, DISABLED,
// or unspecified for a switch the declaration leaves out.
func featureStatus(enabled *bool) Ydb.FeatureFlag_Status {
	switch {
	case enabled == nil:
		return Ydb.FeatureFlag_STATUS_UNSPECIFIED
	case *enabled:
		return Ydb.FeatureFlag_ENABLED
	default:
		return Ydb.FeatureFlag_DISABLED
	}
}

// readReplicas reads the PER_AZ:<n> or ANY_AZ:<n> the reader wrote in
// capitals. A count of zero declares no replicas, which removes the ones a
// table holds.
func readReplicas(written string) (*Ydb_Table.ReadReplicasSettings, error) {
	mode, count, _ := strings.Cut(written, ":")
	n, err := strconv.ParseUint(count, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("read replicas %q are not one YDB takes", written)
	}
	if mode == "PER_AZ" {
		return &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_PerAzReadReplicasCount{PerAzReadReplicasCount: n}}, nil
	}
	return &Ydb_Table.ReadReplicasSettings{Settings: &Ydb_Table.ReadReplicasSettings_AnyAzReadReplicasCount{AnyAzReadReplicasCount: n}}, nil
}

// columnFamily is one declared family. A setting it leaves out stays out, so
// the family keeps what it holds of it.
func columnFamily(family ydbdecl.ColumnFamily) *native.ColumnFamily {
	result := &native.ColumnFamily{Name: family.Name}
	if family.Data != "" {
		result.Data = &Ydb_Table.StoragePool{Media: family.Data}
	}
	switch family.Compression {
	case ydbdecl.CompressionOff:
		result.Compression = Ydb_Table.ColumnFamily_COMPRESSION_NONE
	case ydbdecl.CompressionLZ4:
		result.Compression = Ydb_Table.ColumnFamily_COMPRESSION_LZ4
	}
	switch family.CacheMode {
	case ydbdecl.CacheModeRegular:
		result.CacheMode = native.ColumnFamily_CACHE_MODE_REGULAR
	case ydbdecl.CacheModeInMemory:
		result.CacheMode = native.ColumnFamily_CACHE_MODE_IN_MEMORY
	}
	return result
}

// epochUnits are the units an integer TTL column counts in, as the table
// service numbers them; the column-storage model numbers them alike.
var epochUnits = map[string]Ydb_Table.ValueSinceUnixEpochModeSettings_Unit{
	ydbdecl.Seconds:      Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_SECONDS,
	ydbdecl.Milliseconds: Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_MILLISECONDS,
	ydbdecl.Microseconds: Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_MICROSECONDS,
	ydbdecl.Nanoseconds:  Ydb_Table.ValueSinceUnixEpochModeSettings_UNIT_NANOSECONDS,
}

// rowTTL is a TTL that only deletes rows as the table service takes it: a
// date or time column without a unit, an integer column counting since the
// Unix epoch with one, and the interval in whole seconds.
func rowTTL(policy ydbdecl.TTL) (*Ydb_Table.TtlSettings, error) {
	seconds, err := ydbdecl.IntervalSeconds(policy.Interval)
	if err != nil {
		return nil, err
	}
	// IntervalSeconds bounds the interval by YDB's Interval type, which fits
	// 32 bits.
	expire := uint32(seconds)
	if policy.Unit == "" {
		return &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_DateTypeColumn{
			DateTypeColumn: &Ydb_Table.DateTypeColumnModeSettings{ColumnName: policy.Column, ExpireAfterSeconds: expire}}}, nil
	}
	return &Ydb_Table.TtlSettings{Mode: &Ydb_Table.TtlSettings_ValueSinceUnixEpoch{
		ValueSinceUnixEpoch: &Ydb_Table.ValueSinceUnixEpochModeSettings{ColumnName: policy.Column, ColumnUnit: epochUnits[policy.Unit], ExpireAfterSeconds: expire}}}, nil
}

// columnStorageIntent is a column table's storage as the column-storage model
// holds it. A TTL that only deletes is the storage's one deleting tier, as a
// YAML or Go source's row_deletion properties beside a column table are. A
// hash key or a shard count the declaration leaves out is the server's choice.
func columnStorageIntent(facets ydbdecl.TableFacets) (*native.ColumnStorageIntent, error) {
	if facets.Partitioning != nil || len(facets.Families) != 0 {
		return nil, errors.New(schema.ColumnTableRowSettingsRefusal)
	}
	store := facets.ColumnStore
	intent := &native.ColumnStorageIntent{HashColumns: slices.Clone(store.HashColumns), Partitions: store.Partitions}
	switch {
	case store.TTL != nil && facets.TTL != nil:
		return nil, errors.New(schema.ColumnTableBothTTLRefusal)
	case store.TTL != nil:
		enabled := &native.ColumnTTLEnabled{Column: store.TTL.Column, ColumnUnit: uint64(epochUnits[store.TTL.Unit])}
		for position, tier := range store.TTL.Tiers {
			seconds, err := ydbdecl.IntervalSeconds(tier.Interval)
			if err != nil {
				return nil, fmt.Errorf("TTL tier %d: %w", position+1, err)
			}
			step := &native.ColumnTTLTier{ApplyAfterSeconds: seconds, Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}}
			if tier.ExternalSource != "" {
				step.Action = &native.ColumnTTLTier_ExternalStorage{ExternalStorage: tier.ExternalSource}
			}
			enabled.Tiers = append(enabled.Tiers, step)
		}
		intent.Ttl = &native.ColumnTTLPolicy{State: &native.ColumnTTLPolicy_Enabled{Enabled: enabled}}
	case facets.TTL != nil:
		seconds, err := ydbdecl.IntervalSeconds(facets.TTL.Interval)
		if err != nil {
			return nil, err
		}
		intent.Ttl = &native.ColumnTTLPolicy{State: &native.ColumnTTLPolicy_Enabled{Enabled: &native.ColumnTTLEnabled{Column: facets.TTL.Column,
			ColumnUnit: uint64(epochUnits[facets.TTL.Unit]),
			Tiers:      []*native.ColumnTTLTier{{ApplyAfterSeconds: seconds, Action: &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}}}}}}
	}
	return intent, nil
}
