// Package columnstore decodes the monitoring description of a YDB column table
// together with the table service's description, without network access.
//
// The table service's description of a column table carries its columns, its
// key, its hash key and its TTL, and leaves out the shard count and the local
// indexes; the monitoring endpoint's scheme description carries all of them.
// Decode reads both and refuses a disagreement, so neither page alone decides
// what a column table is.
package columnstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
)

// Page is the monitoring page a column table's description is read from, with
// the parameters path (the table's absolute path) and database.
const Page = "/viewer/json/describe"

// consistentHash is the hash function YQL's CREATE TABLE ... PARTITION BY HASH
// writes, HASH_FUNCTION_CONSISTENCY_64. The native CreateTable call writes
// HASH_FUNCTION_MODULO_N (0) instead (measured on 25.1.4.7 and 26.2.1.14), and
// the host's embedded engine reads no other function, so neither does the
// provider.
const consistentHash = 2

// Decode checks a captured monitoring description against the table service's
// description before publishing the table's column-storage facet. It returns
// no partial result on disagreement. Native IDs, versions and shard addresses
// are checked and never become schema state.
func Decode(body []byte, fullPath string, table *native.ColumnTableDescribeResult) (*native.ColumnStorageObservation, error) {
	if len(body) > 4<<20 {
		return nil, fmt.Errorf("YDB column description exceeds size limit")
	}
	// The page also carries the path's access lists as raw bytes, which are not
	// UTF-8 (measured on 25.1.4.7 and 26.2.1.14), so only the column table's
	// own description is held to canonical JSON.
	page, err := object(body)
	if err != nil {
		return nil, fmt.Errorf("invalid YDB monitoring response")
	}
	var status, describedPath string
	if json.Unmarshal(page["Status"], &status) != nil || json.Unmarshal(page["Path"], &describedPath) != nil ||
		status != "StatusSuccess" || describedPath != fullPath || fullPath == "/" || !path.IsAbs(fullPath) || path.Clean(fullPath) != fullPath {
		return nil, fmt.Errorf("YDB monitoring response does not describe the requested path")
	}
	description, err := object(page["PathDescription"])
	if err != nil {
		return nil, fmt.Errorf("missing YDB path description")
	}
	canonical, err := modeljson.Canonical(description["ColumnTableDescription"])
	if err != nil {
		return nil, fmt.Errorf("invalid YDB column description JSON")
	}
	var source native.MonitoringColumnTable
	if err := protojson.Unmarshal(canonical, &source); err != nil {
		return nil, fmt.Errorf("invalid or unsupported YDB column description")
	}
	if err := schema.Known(table); err != nil {
		return nil, err
	}
	if table.GetStoreType() != Ydb_Table.StoreType_STORE_TYPE_COLUMN || table.GetSelf().GetType() != Ydb_Scheme.Entry_COLUMN_TABLE ||
		table.GetSelf().GetName() != path.Base(fullPath) || source.Name != path.Base(fullPath) {
		return nil, fmt.Errorf("YDB column table identity disagrees across descriptions")
	}
	if err := unmodeled(table); err != nil {
		return nil, err
	}
	if source.IsRestore || source.GetSchema().GetOptions().GetSchemeNeedActualization() {
		return nil, fmt.Errorf("YDB column table schema is not ready")
	}
	ids, names, err := checkColumns(source.GetSchema(), table)
	if err != nil {
		return nil, err
	}
	hash := source.GetSharding().GetHashSharding()
	if hash.GetFunction() != consistentHash || !referencesKnown(hash.GetColumns(), names) {
		return nil, fmt.Errorf("invalid or unsupported YDB column hash sharding")
	}
	if !slices.Equal(table.GetPartitioningSettings().GetPartitionBy(), hash.Columns) {
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
	if err := checkTTL(table.GetTtlSettings(), result.Ttl); err != nil {
		return nil, err
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

// object reads one JSON object into its members and refuses a member written
// twice, which a map would read as the last one silently.
func object(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	members := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, _ := token.(string)
		if _, repeated := members[key]; repeated {
			return nil, fmt.Errorf("member %q is written twice", key)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		members[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after the JSON object")
	}
	return members, nil
}

// unmodeled refuses a table-service description that states a setting the
// column-storage model has no place for: the provider would read the table as
// one without it and plan it away. A column table holds no global index,
// changefeed or column family, and its partitioning is its hash key.
func unmodeled(table *native.ColumnTableDescribeResult) error {
	partitioning := proto.CloneOf(table.GetPartitioningSettings())
	if partitioning != nil {
		partitioning.PartitionBy = nil
	}
	switch {
	case len(table.ShardKeyBounds)+len(table.Indexes)+len(table.ColumnFamilies)+len(table.Changefeeds) != 0:
		return fmt.Errorf("YDB column table description carries indexes, families, changefeeds or shard bounds the provider does not model")
	case table.StorageSettings != nil || table.ReadReplicasSettings != nil || table.KeyBloomFilter != Ydb.FeatureFlag_STATUS_UNSPECIFIED ||
		table.Tiering != "" || table.Temporary || len(table.Attributes) != 0 || (partitioning != nil && !proto.Equal(partitioning, &Ydb_Table.PartitioningSettings{})):
		return fmt.Errorf("YDB column table description carries settings the provider does not model")
	case table.GetTtlSettings().GetRunIntervalSeconds() != 0:
		return fmt.Errorf("YDB column table TTL carries a run interval the provider does not model")
	}
	return nil
}

func checkColumns(source *native.MonitoringColumnSchema, table *native.ColumnTableDescribeResult) (map[uint64]string, map[string]bool, error) {
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
		if nativeColumn.GetFamily() != "" || nativeColumn.GetDefaultValue() != nil || nativeColumn.Compression != nil || len(nativeColumn.Encoding) != 0 {
			return nil, nil, fmt.Errorf("configured YDB column storage is not modeled")
		}
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

// tier is one step of a TTL as either description states it.
type tier struct {
	column  string
	unit    uint64
	seconds uint64
	storage string
	delete  bool
}

// checkTTL requires the table service's TTL to be the one the monitoring
// description reports. The table service writes a TTL that only deletes as a
// date or epoch mode and any other as tiers; both read the same as tiers here.
func checkTTL(described *native.TtlSettings, observed *native.ColumnTTLPolicy) error {
	var held []tier
	switch mode := described.GetMode().(type) {
	case nil:
	case *native.TtlSettings_DateTypeColumn:
		held = []tier{{column: mode.DateTypeColumn.GetColumnName(), seconds: uint64(mode.DateTypeColumn.GetExpireAfterSeconds()), delete: true}}
	case *native.TtlSettings_ValueSinceUnixEpoch:
		held = []tier{{column: mode.ValueSinceUnixEpoch.GetColumnName(), unit: uint64(mode.ValueSinceUnixEpoch.GetColumnUnit()),
			seconds: uint64(mode.ValueSinceUnixEpoch.GetExpireAfterSeconds()), delete: true}}
	case *native.TtlSettings_TieredTtl:
		for _, step := range mode.TieredTtl.GetTiers() {
			var read tier
			switch expression := step.GetExpression().(type) {
			case *native.TtlTier_DateTypeColumn:
				read = tier{column: expression.DateTypeColumn.GetColumnName(), seconds: uint64(expression.DateTypeColumn.GetExpireAfterSeconds())}
			case *native.TtlTier_ValueSinceUnixEpoch:
				read = tier{column: expression.ValueSinceUnixEpoch.GetColumnName(), unit: uint64(expression.ValueSinceUnixEpoch.GetColumnUnit()),
					seconds: uint64(expression.ValueSinceUnixEpoch.GetExpireAfterSeconds())}
			default:
				return fmt.Errorf("YDB column TTL tier has no expression")
			}
			switch action := step.GetAction().(type) {
			case *native.TtlTier_Delete:
				read.delete = true
			case *native.TtlTier_EvictToExternalStorage:
				read.storage = action.EvictToExternalStorage.GetStorage()
			default:
				return fmt.Errorf("YDB column TTL tier has no action")
			}
			held = append(held, read)
		}
	}
	var reported []tier
	if enabled := observed.GetEnabled(); enabled != nil {
		for _, step := range enabled.GetTiers() {
			reported = append(reported, tier{column: enabled.Column, unit: enabled.ColumnUnit, seconds: step.ApplyAfterSeconds,
				storage: step.GetExternalStorage(), delete: step.GetDelete() != nil})
		}
	}
	if !slices.Equal(held, reported) {
		return fmt.Errorf("YDB column TTL disagrees across descriptions")
	}
	return nil
}
