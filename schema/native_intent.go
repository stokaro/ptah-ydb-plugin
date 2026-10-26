package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// NativeDesiredModel returns a declaration contract for native objects, indexes,
// changefeeds, and column storage. Unsupported kinds return nil.
func NativeDesiredModel(kind string) *protocol.ValueModel {
	return proto.CloneOf(nativeIntentDefinitions[kind])
}

var nativeIntentDefinitions = map[string]*protocol.ValueModel{
	IndexProperties: buildNativeIntentModel(IndexProperties), ChangefeedProperties: buildNativeIntentModel(ChangefeedProperties), TopicProperties: buildNativeIntentModel(TopicProperties),
	ViewProperties: buildNativeIntentModel(ViewProperties), ExternalSourceProperties: buildNativeIntentModel(ExternalSourceProperties), ExternalTableProperties: buildNativeIntentModel(ExternalTableProperties), ColumnStorageProperties: buildNativeIntentModel(ColumnStorageProperties),
	ResourcePoolProperties: buildNativeIntentModel(ResourcePoolProperties), ResourcePoolClassifierProperties: buildNativeIntentModel(ResourcePoolClassifierProperties),
	StreamingQueryProperties: buildNativeIntentModel(StreamingQueryProperties),
}

func nativeIntentMessage(kind string) proto.Message {
	switch kind {
	case IndexProperties:
		return &native.IndexSetIntent{}
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
		"index_implementation": "complete typed requirements on server-owned implementation tables, including columns and native settings; not independent table declarations; mutable internal sequence positions are excluded",
		"index_status":         "declarations require a ready implementation; conversion to observed form predicts readiness without establishing coverage",
		"streams":              "all topic and consumer configuration retained without partition topology or statistics; access uses separate direct-grant and effective-access requirements",
		"initial_scan":         "initialScan retains creation intent; disabled is independent; observed INITIAL_SCAN converts to enabled intent with initialScan true, never to a claim that scanning finished",
		"conversion":           "offline and all-or-nothing; requires complete implementation and child-topic settings; a model is compared and planned only where the provider names a service for it",
		"access":               "missing access leaves ownership and permissions unmanaged; present access retains complete direct grants and effective-permission requirements without granting permissions",
		"native_objects":       "views require matching access from view and table APIs; external options, references and column order remain exact; no network access to external sources",
		"topic_declarations":   "a topic setting or consumer setting left at zero is one the declaration leaves out, which requests the value YDB gives a new topic; attributes and access are not declared",
		"column_storage":       "preserves hash order, partition and data-channel counts, local indexes and TTL tiers; excludes native IDs and runtime topology; a declaration may omit the hash key, the shard count and the data channels, which the server then chooses, and states the whole TTL of the table, a TTL that only deletes as one deleting tier; the server's derived expiration is not declared",
	}
	if IsWorkloadObject(kind) {
		definition["workload_objects"] = workloadDefinition
	}
	if kind == StreamingQueryProperties {
		definition["streaming_queries"] = streamingDefinition
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
	result := &protocol.ValueModel{Kind: kind, DisplayName: modelDisplayName(kind), Representation: protocol.ValueInput_DESIRED, SchemaVersion: 1, Encoding: "ydb-protojson", DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]), CanonicalFormat: kind + "/desired-json-v1"}
	switch kind {
	case ColumnStorageProperties, TopicProperties, ExternalSourceProperties, ExternalTableProperties, ResourcePoolProperties, ResourcePoolClassifierProperties, StreamingQueryProperties:
		// A YAML, Go or SQL source says whether a table stores columns, so the
		// absence of the facet on a table such a source describes is a request
		// for row storage. Each of them declares topics, external data sources
		// and external tables too, and describes them completely, as for the
		// host's embedded engine.
		// Resource pools and classifiers are also declared by these formats;
		// a source that leaves one out keeps it (see the source service).
		result.SourceFormats = []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_SQL}
	case ChangefeedProperties:
		// A YAML or Go source declares the changefeeds of each table it
		// declares, as for the host's embedded engine, so a table it declares
		// with none is declared to have none.
		result.SourceFormats = []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS}
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
		for _, feed := range intent.Changefeeds {
			sortTopicIntent(feed.Topic)
		}
	case *native.TopicIntent:
		sortTopicIntent(intent)
	case *native.ColumnStorageIntent:
		slices.SortFunc(intent.LocalIndexes, func(a, b *native.LocalIndexObservation) int { return strings.Compare(a.Name, b.Name) })
	}
	payload, err := protojson.Marshal(target)
	if err != nil {
		return nil, err
	}
	payload, err = modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = payload
	return result, nil
}

func sortTopicIntent(intent *native.TopicIntent) {
	slices.SortFunc(intent.Consumers, func(a, b *native.ConsumerIntent) int { return strings.Compare(a.Name, b.Name) })
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
		return convertedEnvelope(value, model(value.Kind), observed, Normalize)
	}
	return nil, fmt.Errorf("native YDB conversion requires distinct schema representations")
}

func nativeIntent(value proto.Message) (proto.Message, error) {
	switch v := value.(type) {
	case *native.IndexObservations:
		return indexIntents(v)
	case *native.ChangefeedObservations:
		return changefeedIntents(v), nil
	case *native.DescribeTopicResult:
		return topicIntent(v), nil
	case *native.ResourcePoolSettings, *native.ResourcePoolClassifierSettings:
		return proto.Clone(v), nil
	case *native.StreamingQuerySettings:
		return &native.StreamingQueryIntent{Text: v.GetText(), Run: v.Run, ResourcePool: v.GetResourcePool()}, nil
	default:
		return objectIntent(value)
	}
}

func nativeObservation(value proto.Message) (proto.Message, error) {
	switch v := value.(type) {
	case *native.IndexSetIntent:
		return indexObservations(v)
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
	default:
		return objectObservation(value)
	}
}
