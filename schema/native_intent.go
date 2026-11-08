package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
)

// NativeDesiredModel returns a declaration contract for native objects, indexes,
// changefeeds, and column storage. Unsupported kinds return nil.
func NativeDesiredModel(kind string) *protocol.ValueModel {
	return proto.CloneOf(nativeIntentDefinitions[kind])
}

var nativeIntentDefinitions = map[string]*protocol.ValueModel{
	SecretProperties: buildNativeIntentModel(SecretProperties),
	IndexProperties:  buildNativeIntentModel(IndexProperties), ChangefeedProperties: buildNativeIntentModel(ChangefeedProperties), TopicProperties: buildNativeIntentModel(TopicProperties),
	ViewProperties: buildNativeIntentModel(ViewProperties), ExternalSourceProperties: buildNativeIntentModel(ExternalSourceProperties),
	ExternalTableProperties: buildNativeIntentModel(ExternalTableProperties), ColumnStorageProperties: buildNativeIntentModel(ColumnStorageProperties),
	ResourcePoolProperties: buildNativeIntentModel(ResourcePoolProperties), ResourcePoolClassifierProperties: buildNativeIntentModel(ResourcePoolClassifierProperties),
	StreamingQueryProperties: buildNativeIntentModel(StreamingQueryProperties),
	ReplicationProperties:    buildNativeIntentModel(ReplicationProperties), TransferProperties: buildNativeIntentModel(TransferProperties),
	IndexSettingsProperties: buildNativeIntentModel(IndexSettingsProperties),
}

func nativeIntentMessage(kind string) proto.Message {
	switch kind {
	case SecretProperties:
		return &native.SecretIntent{}
	case IndexProperties:
		return &native.IndexSetIntent{}
	case IndexSettingsProperties:
		return &native.IndexSettingsIntent{}
	case ChangefeedProperties:
		return &native.ChangefeedSetIntent{}
	case TopicProperties:
		return &native.TopicIntent{}
	case ViewProperties:
		return &native.ViewIntent{}
	case ExternalSourceProperties:
		return &native.ExternalSourceIntent{}
	case ExternalTableProperties:
		return &native.ExternalTableIntent{}
	case ColumnStorageProperties:
		return &native.ColumnStorageIntent{}
	case ResourcePoolProperties:
		return &native.ResourcePoolSettings{}
	case ResourcePoolClassifierProperties:
		return &native.ResourcePoolClassifierSettings{}
	case StreamingQueryProperties:
		return &native.StreamingQueryIntent{}
	case ReplicationProperties:
		return &native.AsyncReplicationIntent{}
	case TransferProperties:
		return &native.TransferIntent{}
	default:
		return nil
	}
}

func buildNativeIntentModel(kind string) *protocol.ValueModel {
	target := nativeIntentMessage(kind)
	graph := make(map[string]any)
	describeMessage(target.ProtoReflect().Descriptor(), graph)
	definition := map[string]any{
		"kind": kind, "representation": "desired", "schema_version": 1, "encoding": "ydb-protojson",
		"message": string(target.ProtoReflect().Descriptor().FullName()), "messages": graph,
		"unknown_fields_and_enum_values": "refused", "null_settings": "refused recursively except native typed null literals",
		"canonicalization":     "protojson followed by modeljson canonical JSON; indexes, implementations, feeds and consumers sorted by name; columns and keys retain order",
		"index_implementation": "complete typed requirements on server-owned implementation tables, with columns and native settings, or none for a declared vector or full-text index; not independent tables; mutable sequence positions excluded",
		"index_status":         "declarations require a ready implementation; conversion to observed form predicts readiness without establishing coverage",
		"streams":              "all topic and consumer configuration retained without partition topology or statistics; access uses separate direct-grant and effective-access requirements",
		"initial_scan":         "initialScan retains creation intent; disabled is independent; observed INITIAL_SCAN converts to enabled intent with initialScan true, never to a claim that scanning finished",
		"conversion":           "offline and all-or-nothing; requires complete implementation and child-topic settings; a model is compared and planned only where the provider names a service for it",
		"access":               "missing access leaves ownership and permissions unmanaged; present access retains complete direct grants and effective-permission requirements without granting permissions",
		"native_objects":       "views require matching access from view and table APIs; external options, references and column order remain exact; no network access to external sources",
		"topic_declarations":   "a topic setting or consumer setting left at zero is one the declaration leaves out, which requests the value YDB gives a new topic; attributes and access are not declared",
		"column_storage": "preserves hash order, partition and data-channel counts, local indexes and TTL tiers; excludes native IDs and runtime topology; " +
			"a declaration may omit the hash key, the shard count and the data channels, which the server then chooses, and states the whole TTL of the table, " +
			"a TTL that only deletes as one deleting tier; the server's derived expiration is not declared",
	}
	if kind == IndexSettingsProperties {
		definition["index_settings"] = indexSettingsDeclaration
	}
	if kind == TopicProperties {
		// A topic's own definition says what its canonical form keeps; the
		// shared text above stays as it is for the other kinds, whose
		// identities it is part of.
		definition["canonicalization"] = topicCanonicalization
	}
	if kind == ChangefeedProperties {
		// The same holds for a changefeed's topic (see sortNativeIntent).
		definition["canonicalization"] = changefeedCanonicalization
	}
	if kind == SecretProperties {
		definition["secrets"] = "value_env is a required PTAH_SECRET_ approved-input reference; observed presence carries no value or value reference; " +
			"conversions derive a deterministic reference from the exact path; rotations require an explicit comparison request"
	}
	if IsWorkloadObject(kind) {
		definition["workload_objects"] = workloadDefinition
	}
	if kind == StreamingQueryProperties {
		definition["streaming_queries"] = streamingDefinition
	}
	if kind == ReplicationProperties || kind == TransferProperties {
		definition["replication_objects"] = replicationDeclaration
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
	result := &protocol.ValueModel{Kind: kind, DisplayName: modelDisplayName(kind), Representation: protocol.ValueInput_DESIRED, SchemaVersion: 1, Encoding: "ydb-protojson",
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]), CanonicalFormat: kind + "/desired-json-v1"}
	switch kind {
	case ReplicationProperties, TransferProperties:
		// A YAML, Go or SQL source declares replications and transfers at
		// document scope, as for the host's embedded engine, and describes
		// them completely.
		result.SourceFormats = []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_SQL}
	case SecretProperties, ColumnStorageProperties, TopicProperties, ExternalSourceProperties, ExternalTableProperties, ResourcePoolProperties, ResourcePoolClassifierProperties, StreamingQueryProperties:
		// A YAML, Go or SQL source says whether a table stores columns, so the
		// absence of the facet on a table such a source describes is a request
		// for row storage. Each of them declares topics, external data sources
		// and external tables too, and describes them completely, as for the
		// host's embedded engine.
		// Resource pools and classifiers are also declared by these formats;
		// a source that leaves one out keeps it (see the source service).
		result.SourceFormats = []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_SQL}
	case ChangefeedProperties:
		// A YAML, Go or SQL source declares the changefeeds of each table it
		// declares, as for the host's embedded engine, so a table it declares
		// with none is declared to have none. A SQL reading carries them as a
		// facet of the table only for a host that takes one
		// (provider.SQLFacetsFeature).
		result.SourceFormats = []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_SQL}
	case IndexProperties:
		// A YAML or Go source declares the vector and full-text indexes of a
		// table it declares as index directives or entries of their types,
		// which a host that negotiates source.index-kinds sends whole; a
		// table that declares none states an empty set, which an export
		// writes with the table's declaration. A YQL CREATE TABLE declares
		// them in its INDEX clauses, and a SQL reading carries them as a
		// facet of the table for a host that takes one and negotiates
		// source.index-kinds (yqlschema.Reply).
		result.SourceFormats = []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_SQL}
	case IndexSettingsProperties:
		// The index directive of a Go source, the index entry of a YAML
		// source, the index block of an HCL document and the WITH clause of
		// an index in a YQL CREATE TABLE write the settings as the host's
		// embedded engine reads them. A YQL reading carries them as a facet
		// of the index only for a host that takes one (yqlschema.Reply).
		result.SourceFormats = []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_HCL,
			protocol.SourceDocument_SQL}
	}
	return result
}

func normalizeNativeIntent(value *protocol.Envelope) (*protocol.Envelope, error) {
	definition := NativeDesiredModel(value.GetKind())
	target := nativeIntentMessage(value.GetKind())
	if definition == nil || value.GetProviderId() != ProviderID || value.GetSchemaVersion() != definition.SchemaVersion || value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("desired native YDB value does not match its negotiated definition")
	}
	if err := validateModelOwner(value); err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(value.Payload)
	if err != nil {
		return nil, err
	}
	if string(canonical) == "null" {
		return nil, fmt.Errorf("desired native YDB value requires an object")
	}
	if err := rejectNullSettings(canonical, target.ProtoReflect().Descriptor()); err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(canonical, target); err != nil {
		return nil, fmt.Errorf("invalid desired native YDB JSON")
	}
	if err := Known(target); err != nil {
		return nil, err
	}
	if IsWorkloadObject(value.GetKind()) {
		if err := validateWorkload(value.GetOwner(), target); err != nil {
			return nil, err
		}
	}
	if storage, ok := target.(*native.ColumnStorageIntent); ok {
		if err := validateColumnStorageIntent(storage); err != nil {
			return nil, err
		}
	} else if _, err := nativeObservation(target); err != nil {
		return nil, err
	}
	sortNativeIntent(target)
	payload, err := protojson.Marshal(target)
	if err != nil {
		return nil, err
	}
	payload, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	requireStreamingContext(result)
	result.Payload = payload
	return result, nil
}

// sortNativeIntent sorts by name the lists the canonical form of a
// declaration keeps in name order: indexes and their implementation tables,
// changefeeds, and local indexes. A topic, and the topic of a changefeed,
// keeps its consumers in the order the declaration lists them, as the host's
// embedded engine wrote them into CREATE TOPIC and ALTER TOPIC: measured on
// 25.1.4.7 and 26.2.1.14 at 74de0b65b, a new changefeed declared with
// consumers zz, aa and mm took `ADD CONSUMER zz`, `ADD CONSUMER aa` and `ADD
// CONSUMER mm` in that order.
func sortNativeIntent(target proto.Message) {
	switch intent := target.(type) {
	case *native.IndexSetIntent:
		slices.SortFunc(intent.Indexes, func(a, b *native.IndexDeclaration) int {
			return strings.Compare(a.Specification.Name, b.Specification.Name)
		})
		for _, index := range intent.Indexes {
			slices.SortFunc(index.Implementation, func(a, b *native.ImplementationRequirement) int { return strings.Compare(a.Name, b.Name) })
		}
	case *native.ChangefeedSetIntent:
		slices.SortFunc(intent.Changefeeds, func(a, b *native.ChangefeedIntent) int { return strings.Compare(a.Name, b.Name) })
	case *native.ColumnStorageIntent:
		slices.SortFunc(intent.LocalIndexes, func(a, b *native.LocalIndexObservation) int { return strings.Compare(a.Name, b.Name) })
	}
}

// ConvertNative converts native models without publishing coverage or
// confusing an initial scan request with its current progress.
func ConvertNative(value *protocol.Envelope, from, to protocol.ValueInput_Representation) (*protocol.Envelope, error) {
	if nativeIntentMessage(value.GetKind()) == nil {
		return nil, fmt.Errorf("unsupported native YDB model")
	}
	if from == protocol.ValueInput_OBSERVED && to == protocol.ValueInput_DESIRED {
		normalized, err := Normalize(value)
		if err != nil {
			return nil, err
		}
		observed := message(value.Kind)
		if err := protojson.Unmarshal(normalized.Payload, observed); err != nil {
			return nil, err
		}
		intent, err := nativeIntent(observed)
		if err != nil {
			return nil, err
		}
		if secret, ok := intent.(*native.SecretIntent); ok {
			secret.ValueEnv = secrets.DefaultValueEnv(value.Owner.GetSchema(), value.Owner.GetName())
		}
		if err := convertExternalContext(value, intent, to); err != nil {
			return nil, err
		}
		return convertedEnvelope(value, NativeDesiredModel(value.Kind), intent, NormalizeDesired)
	}
	if from == protocol.ValueInput_DESIRED && to == protocol.ValueInput_OBSERVED {
		normalized, err := normalizeNativeIntent(value)
		if err != nil {
			return nil, err
		}
		intent := nativeIntentMessage(value.Kind)
		if err := protojson.Unmarshal(normalized.Payload, intent); err != nil {
			return nil, err
		}
		observed, err := nativeObservation(intent)
		if err != nil {
			return nil, err
		}
		if err := convertExternalContext(value, observed, to); err != nil {
			return nil, err
		}
		return convertedEnvelope(value, model(value.Kind), observed, Normalize)
	}
	return nil, fmt.Errorf("native YDB conversion requires distinct schema representations")
}

func nativeIntent(value proto.Message) (proto.Message, error) {
	switch v := value.(type) {
	case *native.SecretPresence:
		return &native.SecretIntent{}, nil
	case *native.IndexObservations:
		return indexIntents(v)
	case *native.IndexSettingsObservation:
		return indexSettingsIntent(v), nil
	case *native.ChangefeedObservations:
		return changefeedIntents(v), nil
	case *native.DescribeTopicResult:
		return topicIntent(v), nil
	case *native.ResourcePoolSettings, *native.ResourcePoolClassifierSettings:
		return proto.Clone(v), nil
	case *native.StreamingQuerySettings:
		return &native.StreamingQueryIntent{Text: v.GetText(), Run: v.Run, ResourcePool: v.GetResourcePool()}, nil
	case *native.AsyncReplicationSettings, *native.TransferSettings:
		intent, _ := replicationIntent(v)
		return intent, nil
	default:
		return objectIntent(value)
	}
}

func nativeObservation(value proto.Message) (proto.Message, error) {
	switch v := value.(type) {
	case *native.SecretIntent:
		if err := secrets.CheckValueEnv(v.ValueEnv); err != nil {
			return nil, err
		}
		return &native.SecretPresence{}, nil
	case *native.IndexSetIntent:
		return indexObservations(v)
	case *native.IndexSettingsIntent:
		if err := validateIndexSettings(v.GetPartitioningSettings(), v.GetReadReplicasSettings()); err != nil {
			return nil, err
		}
		return indexSettingsObservation(v), nil
	case *native.ChangefeedSetIntent:
		return changefeedObservations(v)
	case *native.TopicIntent:
		if err := validateTopicIntent(v); err != nil {
			return nil, err
		}
		observed := topicObservation(v)
		if err := normalizeTopic(observed); err != nil {
			return nil, err
		}
		return observed, nil
	case *native.ResourcePoolSettings, *native.ResourcePoolClassifierSettings:
		return proto.Clone(v), nil
	case *native.StreamingQueryIntent:
		observed := &native.StreamingQuerySettings{Text: v.GetText(), Run: v.Run, ResourcePool: v.GetResourcePool()}
		if err := normalizeObject(observed); err != nil {
			return nil, err
		}
		return observed, nil
	case *native.AsyncReplicationIntent, *native.TransferIntent:
		observed, _, err := replicationObservation(v)
		return observed, err
	default:
		return objectObservation(value)
	}
}
