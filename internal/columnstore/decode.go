// Package columnstore decodes monitoring descriptions without network access.
package columnstore

import (
	"encoding/json"
	"fmt"
	"path"
	"slices"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Decode checks a captured monitoring description against the table API before
// publishing its storage facet. It returns no partial result on disagreement.
// Native runtime metadata outside ColumnTableDescription is not schema state.
func Decode(body []byte, fullPath string, table *native.DescribeTableResult) (*native.ColumnStorageObservation, error) {
	if len(body) > 4<<20 {
		return nil, fmt.Errorf("YDB column description exceeds size limit")
	}
	canonical, err := modeljson.Canonical(body)
	if err != nil {
		return nil, fmt.Errorf("invalid YDB column description JSON")
	}
	var page map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &page); err != nil {
		return nil, fmt.Errorf("invalid YDB monitoring response")
	}
	var status, describedPath string
	if json.Unmarshal(page["Status"], &status) != nil || json.Unmarshal(page["Path"], &describedPath) != nil ||
		status != "StatusSuccess" || describedPath != fullPath || fullPath == "/" || !path.IsAbs(fullPath) || path.Clean(fullPath) != fullPath {
		return nil, fmt.Errorf("YDB monitoring response does not describe the requested path")
	}
	var description map[string]json.RawMessage
	if err := json.Unmarshal(page["PathDescription"], &description); err != nil {
		return nil, fmt.Errorf("missing YDB path description")
	}
	var source native.MonitoringColumnTable
	if err := protojson.Unmarshal(description["ColumnTableDescription"], &source); err != nil {
		return nil, fmt.Errorf("invalid or unsupported YDB column description")
	}
	if err := schema.Known(table); err != nil {
		return nil, err
	}
	if table.GetStoreType() != Ydb_Table.StoreType_STORE_TYPE_COLUMN || table.GetSelf().GetType() != Ydb_Scheme.Entry_COLUMN_TABLE ||
		table.GetSelf().GetName() != path.Base(fullPath) || source.Name != path.Base(fullPath) {
		return nil, fmt.Errorf("YDB column table identity disagrees across descriptions")
	}
	if source.IsRestore || source.GetSchema().GetOptions().GetSchemeNeedActualization() {
		return nil, fmt.Errorf("YDB column table schema is not ready")
	}
	ids, names, err := checkColumns(source.GetSchema(), table)
	if err != nil {
		return nil, err
	}
	hash := source.GetSharding().GetHashSharding()
	if hash.GetFunction() != 2 || !referencesKnown(hash.GetColumns(), names) {
		return nil, fmt.Errorf("invalid or unsupported YDB column hash sharding")
	}
	if nativeHash := table.GetPartitioningSettings().GetPartitionBy(); len(nativeHash) != 0 && !slices.Equal(nativeHash, hash.Columns) {
		return nil, fmt.Errorf("YDB hash columns disagree across descriptions")
	}
	result := &native.ColumnStorageObservation{
		HashColumns: slices.Clone(hash.Columns), Partitions: source.ColumnShardCount,
		DataChannelCount: source.GetStorageConfig().GetDataChannelCount(),
	}
	indexIDs := map[uint64]bool{}
	for _, index := range source.GetSchema().GetIndexes() {
		if index.Id == 0 || indexIDs[index.Id] {
			return nil, fmt.Errorf("invalid or duplicate YDB local index ID")
		}
		indexIDs[index.Id] = true
		converted, err := convertIndex(index, ids)
		if err != nil {
			return nil, err
		}
		result.LocalIndexes = append(result.LocalIndexes, converted)
	}
	if source.TtlSettings != nil {
		result.Ttl, err = convertTTL(source.TtlSettings, names)
		if err != nil {
			return nil, err
		}
	}
	// Apply the same semantic validation and ordering as offline normalization.
	owner := &protocol.Reference{Engine: "ydb", Database: "/", Kind: "table", Name: source.Name}
	envelope, err := schema.CaptureColumnStorage(owner, result)
	if err != nil {
		return nil, err
	}
	normalized := &native.ColumnStorageObservation{}
	if err := protojson.Unmarshal(envelope.Payload, normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func checkColumns(source *native.MonitoringColumnSchema, table *native.DescribeTableResult) (map[uint64]string, map[string]bool, error) {
	if source == nil || len(source.Columns) == 0 || len(source.Columns) != len(table.Columns) ||
		!slices.Equal(source.KeyColumnNames, table.PrimaryKey) {
		return nil, nil, fmt.Errorf("YDB columns or primary key disagree across descriptions")
	}
	ids, names := map[uint64]string{}, map[string]bool{}
	for i, column := range source.Columns {
		if column.Id == 0 || ids[column.Id] != "" || column.Name == "" || names[column.Name] {
			return nil, nil, fmt.Errorf("invalid or duplicate YDB column identity")
		}
		if column.ColumnFamilyId != 0 || column.StorageId != "" {
			return nil, nil, fmt.Errorf("configured YDB column storage is not modeled")
		}
		nativeColumn := table.Columns[i]
		spelling, nullable, err := schema.ColumnType(nativeColumn.Type)
		if nativeColumn.GetNotNull() {
			nullable = false
		}
		typeValue := nativeColumn.Type
		if typeValue.GetOptionalType() != nil {
			typeValue = typeValue.GetOptionalType().GetItem()
		}
		_, primitive := typeValue.GetType().(*Ydb.Type_TypeId)
		if err != nil || !primitive || column.Name != nativeColumn.Name || column.Type != spelling ||
			column.NotNull == nullable || column.TypeId != uint64(typeValue.GetTypeId()) {
			return nil, nil, fmt.Errorf("YDB column type or nullability disagrees across descriptions")
		}
		ids[column.Id], names[column.Name] = column.Name, true
	}
	if !referencesKnown(source.KeyColumnNames, names) {
		return nil, nil, fmt.Errorf("invalid YDB column primary key")
	}
	for _, family := range source.ColumnFamilies {
		if family.Id != 0 || family.Name != "default" {
			return nil, nil, fmt.Errorf("configured YDB column families are not modeled")
		}
	}
	if len(source.ColumnFamilies) > 1 {
		return nil, nil, fmt.Errorf("duplicate YDB column family")
	}
	return ids, names, nil
}

func referencesKnown(columns []string, known map[string]bool) bool {
	if len(columns) == 0 {
		return false
	}
	seen := map[string]bool{}
	for _, name := range columns {
		if !known[name] || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

func convertIndex(source *native.MonitoringLocalIndex, columns map[uint64]string) (*native.LocalIndexObservation, error) {
	result := &native.LocalIndexObservation{Name: source.Name, StorageId: source.StorageId, InheritPortionStorage: source.InheritPortionStorage}
	var ids []uint64
	switch kind := source.Kind.(type) {
	case *native.MonitoringLocalIndex_BloomFilter:
		if source.ClassName != "BLOOM_FILTER" {
			return nil, fmt.Errorf("YDB bloom index class disagrees with its settings")
		}
		value := kind.BloomFilter
		ids = value.ColumnIds
		result.Kind = &native.LocalIndexObservation_Bloom{Bloom: &native.LocalBloomSettings{
			DataExtractor: value.GetDataExtractor().GetClassName(), BitsStorage: value.GetBitsStorage().GetClassName(),
			FalsePositiveProbability: value.FalsePositiveProbability,
		}}
	case *native.MonitoringLocalIndex_BloomNgrammFilter:
		if source.ClassName != "BLOOM_NGRAMM_FILTER" {
			return nil, fmt.Errorf("YDB ngram index class disagrees with its settings")
		}
		value := kind.BloomNgrammFilter
		ids = []uint64{value.ColumnId}
		result.Kind = &native.LocalIndexObservation_Ngram{Ngram: &native.LocalNGramSettings{
			DataExtractor: value.GetDataExtractor().GetClassName(), BitsStorage: value.GetBitsStorage().GetClassName(),
			FalsePositiveProbability: value.FalsePositiveProbability, NgramSize: value.NgrammSize, CaseSensitive: value.CaseSensitive,
		}}
	case *native.MonitoringLocalIndex_MinMaxIndex:
		if source.ClassName != "MIN_MAX" {
			return nil, fmt.Errorf("YDB min/max index class disagrees with its settings")
		}
		value := kind.MinMaxIndex
		ids = []uint64{value.ColumnId}
		result.Kind = &native.LocalIndexObservation_MinMax{MinMax: &native.LocalMinMaxSettings{DataExtractor: value.GetDataExtractor().GetClassName()}}
	default:
		return nil, fmt.Errorf("unsupported YDB local index kind")
	}
	for _, id := range ids {
		name := columns[id]
		if name == "" {
			return nil, fmt.Errorf("YDB local index references an unknown column")
		}
		result.Columns = append(result.Columns, name)
	}
	return result, nil
}

func convertTTL(source *native.MonitoringTTL, columns map[string]bool) (*native.ColumnTTLPolicy, error) {
	result := &native.ColumnTTLPolicy{}
	switch state := source.State.(type) {
	case *native.MonitoringTTL_Disabled:
		result.State = &native.ColumnTTLPolicy_Disabled{Disabled: &emptypb.Empty{}}
	case *native.MonitoringTTL_Enabled:
		value := state.Enabled
		if !columns[value.ColumnName] {
			return nil, fmt.Errorf("YDB column TTL references an unknown column")
		}
		enabled := &native.ColumnTTLEnabled{Column: value.ColumnName, ColumnUnit: value.ColumnUnit, ExpireAfterSeconds: value.ExpireAfterSeconds}
		for _, tier := range value.Tiers {
			out := &native.ColumnTTLTier{ApplyAfterSeconds: tier.ApplyAfterSeconds}
			switch action := tier.Action.(type) {
			case *native.MonitoringTTLTier_Delete:
				out.Action = &native.ColumnTTLTier_Delete{Delete: &emptypb.Empty{}}
			case *native.MonitoringTTLTier_EvictToExternalStorage:
				out.Action = &native.ColumnTTLTier_ExternalStorage{ExternalStorage: action.EvictToExternalStorage.GetStorage()}
			default:
				return nil, fmt.Errorf("missing YDB column TTL tier action")
			}
			enabled.Tiers = append(enabled.Tiers, out)
		}
		result.State = &native.ColumnTTLPolicy_Enabled{Enabled: enabled}
	default:
		return nil, fmt.Errorf("missing YDB column TTL state")
	}
	return result, nil
}
