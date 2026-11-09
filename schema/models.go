package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Coordination"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"ptah.run/provider-sdk/modeljson"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
)

const modelNamespace = ProviderID + "/"

const (
	// ProviderID binds model envelopes to the official YDB owner.
	ProviderID = "github.com/stokaro/ptah-ydb-plugin"
	// TableProperties identifies observed YDB row-table properties.
	TableProperties = modelNamespace + "row-table-properties"
	// ColumnProperties identifies column family and native default properties.
	ColumnProperties = modelNamespace + "column-properties"
)

// ObservedModels returns fresh definitions for the supported native observations.
func ObservedModels() []*protocol.ValueModel {
	return []*protocol.ValueModel{model(TableProperties), model(ColumnProperties), model(IndexProperties), model(IndexSettingsProperties), model(ChangefeedProperties), model(TopicProperties),
		model(ViewProperties), model(CoordinationProperties), model(ExternalSourceProperties), model(ExternalTableProperties), model(ColumnStorageProperties),
		model(ResourcePoolProperties), model(ResourcePoolClassifierProperties), model(StreamingQueryProperties), model(SecretProperties),
		model(ReplicationProperties), model(TransferProperties)}
}

func message(kind string) proto.Message {
	switch kind {
	case SecretProperties:
		return &native.SecretPresence{}
	case ColumnStorageProperties:
		return &native.ColumnStorageObservation{}
	case TableProperties:
		return &native.DescribeTableResult{}
	case ColumnProperties:
		return &native.ColumnMeta{}
	case IndexProperties:
		return &native.IndexObservations{}
	case IndexSettingsProperties:
		return &native.IndexSettingsObservation{}
	case ChangefeedProperties:
		return &native.ChangefeedObservations{}
	case TopicProperties:
		return &native.DescribeTopicResult{}
	case ViewProperties:
		return &native.ViewObservation{}
	case CoordinationProperties:
		return &Ydb_Coordination.DescribeNodeResult{}
	case ExternalSourceProperties:
		return &Ydb_Table.DescribeExternalDataSourceResult{}
	case ExternalTableProperties:
		return &native.DescribeExternalTableResult{}
	case ResourcePoolProperties:
		return &native.ResourcePoolSettings{}
	case ResourcePoolClassifierProperties:
		return &native.ResourcePoolClassifierSettings{}
	case StreamingQueryProperties:
		return &native.StreamingQuerySettings{}
	case ReplicationProperties:
		return &native.AsyncReplicationSettings{}
	case TransferProperties:
		return &native.TransferSettings{}
	default:
		return nil
	}
}

func excluded(kind string) []string {
	if kind == TableProperties {
		return []string{"columns", "table_stats", "indexes", "changefeeds"}
	}
	if kind == ColumnProperties {
		return []string{"name", "type", "not_null"}
	}
	return nil
}

var definitions = map[string]*protocol.ValueModel{
	SecretProperties:        buildModel(SecretProperties),
	ColumnStorageProperties: buildModel(ColumnStorageProperties),
	TableProperties:         buildModel(TableProperties), ColumnProperties: buildModel(ColumnProperties), IndexProperties: buildModel(IndexProperties),
	ChangefeedProperties: buildModel(ChangefeedProperties), TopicProperties: buildModel(TopicProperties),
	ViewProperties: buildModel(ViewProperties), CoordinationProperties: buildModel(CoordinationProperties), ExternalSourceProperties: buildModel(ExternalSourceProperties), ExternalTableProperties: buildModel(ExternalTableProperties),
	ResourcePoolProperties: buildModel(ResourcePoolProperties), ResourcePoolClassifierProperties: buildModel(ResourcePoolClassifierProperties),
	StreamingQueryProperties: buildModel(StreamingQueryProperties),
	ReplicationProperties:    buildModel(ReplicationProperties), TransferProperties: buildModel(TransferProperties),
	IndexSettingsProperties: buildModel(IndexSettingsProperties),
}

func model(kind string) *protocol.ValueModel { return proto.CloneOf(definitions[kind]) }

func buildModel(kind string) *protocol.ValueModel {
	graph := make(map[string]any)
	describeMessage(message(kind).ProtoReflect().Descriptor(), graph)
	definition := map[string]any{
		"kind": kind, "schema_version": 1, "encoding": "ydb-protojson",
		"message":  string(message(kind).ProtoReflect().Descriptor().FullName()),
		"messages": graph, "excluded_fields": excluded(kind),
		"unknown_fields_and_enum_values": "refused", "representation": "observed",
		"database_binding":        "offline models retain optional database binding; live capture requires a database",
		"canonicalization":        "protojson with absent defaults omitted, then modeljson canonical JSON; arrays retain server order",
		"table_self":              "retain owner and permissions; omit name, type, size_bytes, created_at",
		"common_columns":          "names, storage type, and nullability live only in common columns; primary_key preserves key order",
		"not_observed":            []string{"column_storage", "shared_topic_consumers"},
		"topic_observations":      "retain topic and consumer configuration, attributes and access metadata; omit partitions, topic/consumer stats and transient self fields; consumers sorted by name",
		"changefeed_observations": "complete feed set per table sorted by name; each feed includes its child topic settings; initial scan presence retained, progress counters excluded",
		"native_objects":          "retain view query plus table API attributes; coordination config except duplicate path; external source options and ordered external columns; transient self identity excluded",
		"index_observations":      "all indexes of the owner table; complete implementation tables, none for a declared vector or full-text index; lists sorted by name; column/key order retained; runtime statistics excluded",
	}
	if kind == SecretProperties {
		definition["secrets"] = "presence only; the server never returns the value; owners and access are independently observed common objects"
	}
	if kind == ViewProperties {
		definition["view_api_projection"] = "view table API disabled partitioning-by-size/load switches are nondeclarative scaffolding; omit only this exact configuration and refuse any other partition settings"
	}
	if kind == IndexSettingsProperties {
		definition["index_settings"] = indexSettingsDefinition
	}
	if kind == TopicProperties {
		// A topic's own definition says what its observation keeps; the
		// shared text above stays as it is for the other kinds, whose
		// identities it is part of.
		definition["topic_observations"] = topicObservations
	}
	if kind == ChangefeedProperties {
		definition["topic_observations"] = changefeedObservedDefinition
	}
	if kind == IndexProperties {
		definition["implementation_state"] = "internal sequence positions excluded after unknown-field validation; row storage canonicalized; sequence configuration retained"
	}
	if kind == TableProperties {
		definition["row_storage"] = "unspecified storage on a row-table observation normalizes to STORE_TYPE_ROW"
	}
	if IsWorkloadObject(kind) {
		definition["workload_objects"] = workloadDefinition
	}
	if kind == StreamingQueryProperties {
		definition["streaming_queries"] = streamingDefinition
	}
	if kind == ReplicationProperties || kind == TransferProperties {
		definition["replication_objects"] = replicationDefinition
	}
	if kind == ColumnStorageProperties {
		definition["not_observed"] = []string{"column_table_access", "shared_topic_consumers"}
		definition["column_storage"] = "hash column order and TTL tier order retained; local indexes sorted by name; native IDs and versions excluded; omitted index defaults remain absent; " +
			"a live read combines the table service and the monitoring scheme description"
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(encoded)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: kind, DisplayName: modelDisplayName(kind), Representation: protocol.ValueInput_OBSERVED, SchemaVersion: 1,
		Encoding: "ydb-protojson", DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]),
		CanonicalFormat: kind + "/observed-json-v1"}
}

// describeMessage embeds the complete native field grammar in the negotiated
// model. A dependency update that changes its grammar changes its digest too.
func describeMessage(desc protoreflect.MessageDescriptor, graph map[string]any) {
	name := string(desc.FullName())
	if _, found := graph[name]; found {
		return
	}
	fields := make(map[string]any)
	graph[name] = fields
	for i := 0; i < desc.Fields().Len(); i++ {
		field := desc.Fields().Get(i)
		entry := map[string]any{"number": int(field.Number()), "json_name": field.JSONName(), "kind": field.Kind().String(), "cardinality": field.Cardinality().String(), "presence": field.HasPresence()}
		if field.ContainingOneof() != nil {
			entry["oneof"] = string(field.ContainingOneof().Name())
		}
		if field.Message() != nil {
			entry["message"] = string(field.Message().FullName())
			entry["map"] = field.IsMap()
			describeMessage(field.Message(), graph)
		}
		if field.Enum() != nil {
			values := make(map[string]int32)
			for j := 0; j < field.Enum().Values().Len(); j++ {
				item := field.Enum().Values().Get(j)
				values[string(item.Name())] = int32(item.Number())
			}
			entry["enum"] = values
		}
		fields[string(field.Name())] = entry
	}
}

// CaptureTable separates YDB-only properties from common table and column
// fields. Server statistics and transient self metadata are not schema state.
func CaptureTable(owner *protocol.Reference, description *native.DescribeTableResult) (*protocol.Envelope, error) {
	if err := Known(description); err != nil {
		return nil, err
	}
	if len(description.GetIndexes()) != 0 || len(description.GetChangefeeds()) != 0 {
		return nil, fmt.Errorf("capture child objects separately before table properties")
	}
	projected := proto.CloneOf(description)
	projected.Columns, projected.TableStats = nil, nil
	// Ptah keeps a comment as an attribute of its table. The comment belongs
	// to the comment, which the common wire carries, and never to the
	// table's own declared attributes.
	maps.DeleteFunc(projected.Attributes, func(key, _ string) bool { return ydbcomment.IsKey(key) })
	if len(projected.Attributes) == 0 {
		projected.Attributes = nil
	}
	if projected.Self != nil {
		projected.Self.Name, projected.Self.Type = "", 0
		projected.Self.SizeBytes, projected.Self.CreatedAt = 0, nil
	}
	return capture(TableProperties, owner, projected)
}

// CaptureColumn retains family assignment and typed literal/sequence defaults.
// The common column owns its name, storage type, and nullability.
func CaptureColumn(owner *protocol.Reference, column *native.ColumnMeta) (*protocol.Envelope, error) {
	if err := Known(column); err != nil {
		return nil, err
	}
	projected := proto.CloneOf(column)
	projected.Name, projected.Type, projected.NotNull = "", nil, nil
	return capture(ColumnProperties, owner, projected)
}

func capture(kind string, owner *protocol.Reference, value proto.Message) (*protocol.Envelope, error) {
	if owner.GetDatabase() == "" {
		return nil, fmt.Errorf("captured YDB observation requires a database")
	}
	payload, err := protojson.Marshal(value)
	if err != nil {
		return nil, err
	}
	payload, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	definition := model(kind)
	envelope := &protocol.Envelope{ProviderId: ProviderID, Kind: kind, SchemaVersion: 1, Encoding: definition.Encoding,
		Owner: proto.CloneOf(owner), Payload: payload, DefinitionSha256: definition.DefinitionSha256}
	return Normalize(envelope)
}

// Normalize interprets an observed model without a connection. A payload
// cannot smuggle common fields or unrecognized native fields through a model.
// Offline predictions may remain unbound; capture requires a database because
// a live observation must identify the database that supplied it.
func Normalize(value *protocol.Envelope) (*protocol.Envelope, error) {
	target := message(value.GetKind())
	if target == nil {
		return nil, fmt.Errorf("unknown YDB observation model")
	}
	definition := model(value.GetKind())
	if value.GetProviderId() != ProviderID || value.GetSchemaVersion() != 1 || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("YDB observation does not match its negotiated definition")
	}
	if err := validateModelOwner(value); err != nil {
		return nil, err
	}
	owner := value.GetOwner()
	if err := protojson.Unmarshal(value.GetPayload(), target); err != nil {
		return nil, fmt.Errorf("invalid YDB observation JSON")
	}
	if err := Known(target); err != nil {
		return nil, err
	}
	var refused bool
	target.ProtoReflect().Range(func(field protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		refused = slices.Contains(excluded(value.GetKind()), string(field.Name()))
		return !refused
	})
	if refused {
		return nil, fmt.Errorf("YDB facet repeats a common or transient field")
	}
	if err := normalizeObservation(value.GetKind(), owner, target); err != nil {
		return nil, err
	}
	if strings.TrimSpace(owner.GetName()) == "" {
		return nil, fmt.Errorf("empty YDB observation name")
	}
	payload, err := protojson.Marshal(target)
	if err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	requireStreamingContext(result)
	result.Payload = canonical
	return result, nil
}

// normalizeObservation runs the checks particular to an observation of kind
// and normalizes target in place: column storage, index and changefeed sets,
// native objects, workload objects and row-table properties each have checks
// of their own.
func normalizeObservation(kind string, owner *protocol.Reference, target proto.Message) error {
	if storage, ok := target.(*native.ColumnStorageObservation); ok {
		if err := normalizeColumnStorage(storage); err != nil {
			return err
		}
	}
	if settings, ok := target.(*native.IndexSettingsObservation); ok {
		if err := validateObservedIndexSettings(settings.GetPartitioningSettings(), settings.GetReadReplicasSettings()); err != nil {
			return err
		}
	}
	if indexes, ok := target.(*native.IndexObservations); ok {
		if err := normalizeIndexes(indexes); err != nil {
			return err
		}
	}
	if IsNativeObject(kind) {
		if err := normalizeObject(target); err != nil {
			return err
		}
	}
	if IsWorkloadObject(kind) {
		if err := validateWorkload(owner, target); err != nil {
			return err
		}
	}
	if feeds, ok := target.(*native.ChangefeedObservations); ok {
		if err := normalizeChangefeeds(feeds); err != nil {
			return err
		}
	}
	if table, ok := target.(*native.DescribeTableResult); ok {
		if err := normalizeObservedTable(table); err != nil {
			return err
		}
	}
	return nil
}

// normalizeObservedTable checks a row-table observation for a distinct
// nonempty primary key, no transient identity, row storage and a TTL the
// provider reads, and sets the store type to row, which an unspecified one
// means. A TTL with no mode, or an epoch TTL with no unit, is one a
// declaration cannot state either; the host's embedded engine refused it on
// read.
func normalizeObservedTable(table *native.DescribeTableResult) error {
	if err := validateTableTTL(table.GetTtlSettings()); err != nil {
		return err
	}
	seen := make(map[string]bool)
	if len(table.GetPrimaryKey()) == 0 {
		return fmt.Errorf("observed YDB table properties require a primary key")
	}
	for _, key := range table.GetPrimaryKey() {
		if key == "" || seen[key] {
			return fmt.Errorf("invalid YDB primary key")
		}
		seen[key] = true
	}
	if transientSelf(table.GetSelf()) {
		return fmt.Errorf("YDB facet repeats transient table identity")
	}
	if table.GetStoreType() != Ydb_Table.StoreType_STORE_TYPE_ROW && table.GetStoreType() != Ydb_Table.StoreType_STORE_TYPE_UNSPECIFIED {
		return fmt.Errorf("column storage requires its own description")
	}
	table.StoreType = Ydb_Table.StoreType_STORE_TYPE_ROW
	return nil
}

// Models returns negotiated models with their separate representations. Source
// and change models retain their own canonical formats.
func Models() []*protocol.ValueModel {
	return append(ObservedModels(),
		NativeDesiredModel(SecretProperties), SecretChangeModel(), SecretOperationModel(),
		TableDesiredModel(), ColumnDesiredModel(),
		NativeDesiredModel(IndexProperties), NativeDesiredModel(IndexSettingsProperties), NativeDesiredModel(ChangefeedProperties), NativeDesiredModel(TopicProperties), NativeDesiredModel(ViewProperties),
		NativeDesiredModel(ExternalSourceProperties), NativeDesiredModel(ExternalTableProperties), NativeDesiredModel(ColumnStorageProperties),
		CoordinationDesiredModel(), CoordinationChangeModel(), CoordinationOperationModel(),
		TopicChangeModel(), TopicOperationModel(),
		ExternalSourceChangeModel(), ExternalSourceOperationModel(), ExternalTableChangeModel(), ExternalTableOperationModel(),
		TablePropertiesChangeModel(), ColumnStorageChangeModel(), ChangefeedOperationModel(),
		NativeDesiredModel(ResourcePoolProperties), NativeDesiredModel(ResourcePoolClassifierProperties),
		ResourcePoolChangeModel(), ResourcePoolClassifierChangeModel(), WorkloadOperationModel(),
		NativeDesiredModel(StreamingQueryProperties), StreamingQueryChangeModel(), StreamingQueryOperationModel(),
		NativeDesiredModel(ReplicationProperties), ReplicationChangeModel(), ReplicationOperationModel(),
		NativeDesiredModel(TransferProperties), TransferChangeModel(), TransferOperationModel(),
	)
}

func validateModelOwner(value *protocol.Envelope) error {
	owner := value.GetOwner()
	if value.GetKind() == SecretProperties {
		if err := validateSecretOwner(owner); err != nil {
			return err
		}
	}
	if IsWorkloadObject(value.GetKind()) {
		if owner.GetKind() != value.GetKind() {
			return fmt.Errorf("invalid YDB observation owner")
		}
		return ValidateWorkloadOwner(owner)
	}
	expected := "table"
	if IsNativeObject(value.GetKind()) {
		expected = value.GetKind()
	}
	switch value.GetKind() {
	case ColumnProperties:
		expected = "column"
	case IndexSettingsProperties:
		expected = "index"
	}
	if owner.GetEngine() != "ydb" || owner.GetKind() != expected || owner.GetName() == "" || owner.GetCatalog() != "" || len(owner.GetOverload()) != 0 {
		return fmt.Errorf("invalid YDB observation owner")
	}
	// A column and an index name their table as the only path component.
	part := expected == "column" || expected == "index"
	if (!part && len(owner.GetPath()) != 0) || (part && len(owner.GetPath()) != 1) {
		return fmt.Errorf("invalid YDB observation owner path")
	}
	// A standalone object's statements write its directory and its name as
	// one path, so a name that holds a slash would name another object.
	if IsNativeObject(value.GetKind()) {
		return ValidateNativeOwner(owner)
	}
	return nil
}

func modelDisplayName(kind string) string {
	switch kind {
	case SecretProperties:
		return "YDB secrets"
	case TableProperties:
		return "YDB row-table properties"
	case ColumnProperties:
		return "YDB column properties"
	case IndexProperties:
		return "YDB index sets"
	case IndexSettingsProperties:
		return "YDB index properties"
	case ChangefeedProperties:
		return "YDB changefeed sets"
	case TopicProperties:
		return "YDB topics"
	case ViewProperties:
		return "YDB views"
	case ExternalSourceProperties:
		return "YDB external data sources"
	case ExternalTableProperties:
		return "YDB external tables"
	case CoordinationProperties:
		return "YDB coordination nodes"
	case ColumnStorageProperties:
		return "YDB column-storage properties"
	case ResourcePoolProperties:
		return "YDB resource pools"
	case ResourcePoolClassifierProperties:
		return "YDB resource pool classifiers"
	case StreamingQueryProperties:
		return "YDB streaming queries"
	case ReplicationProperties:
		return "YDB async replications"
	case TransferProperties:
		return "YDB transfers"
	default:
		return ""
	}
}
