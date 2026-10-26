package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ColumnDesiredModel describes writable column settings independently from
// server observations. Common identity, type and nullability remain outside it.
func ColumnDesiredModel() *protocol.ValueModel { return proto.CloneOf(columnDesiredModel) }

var columnDesiredModel = buildColumnDesiredModel()

func buildColumnDesiredModel() *protocol.ValueModel {
	graph := make(map[string]any)
	describeMessage((&native.ColumnIntent{}).ProtoReflect().Descriptor(), graph)
	definition := map[string]any{
		"kind": ColumnProperties, "representation": "desired", "schema_version": 1,
		"encoding": "ydb-protojson", "message": "ptah.ydb.native.ColumnIntent", "messages": graph,
		"unknown_fields_and_enum_values": "refused", "null_settings": "refused",
		"canonicalization": "protojson followed by modeljson canonical JSON; encoding order retained",
		"default_intent":   "absent default requests the native no-default setting; normalization preserves omission; dropDefault must be true; literal and sequence are mutually exclusive",
		"sequence":         "configuration only; mutable setVal observations cannot be exported as declarations",
		"conversion":       "retains family, literal type and value, sequence configuration, compression and encoding; establishes no inspection coverage",
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
	return &protocol.ValueModel{Kind: ColumnProperties, DisplayName: modelDisplayName(ColumnProperties), Representation: protocol.ValueInput_DESIRED, SchemaVersion: 1,
		Encoding: "ydb-protojson", DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(digest[:]),
		CanonicalFormat: ColumnProperties + "/desired-json-v1", SourceFormats: []protocol.SourceDocument_Format{protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_YAML, protocol.SourceDocument_SQL}}
}

func normalizeColumnIntent(value *protocol.Envelope) (*protocol.Envelope, error) {
	definition := ColumnDesiredModel()
	if value.GetProviderId() != ProviderID || value.GetKind() != definition.Kind || value.GetSchemaVersion() != definition.SchemaVersion ||
		value.GetEncoding() != definition.Encoding || value.GetDefinitionSha256() != definition.DefinitionSha256 {
		return nil, fmt.Errorf("desired YDB column does not match its negotiated definition")
	}
	if err := validateModelOwner(value); err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(value.GetPayload())
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("desired YDB column requires an object")
	}
	var intent native.ColumnIntent
	if err := rejectNullSettings(canonical, intent.ProtoReflect().Descriptor()); err != nil {
		return nil, err
	}
	if err := protojson.Unmarshal(canonical, &intent); err != nil {
		return nil, fmt.Errorf("invalid desired YDB column JSON")
	}
	if err := Known(&intent); err != nil {
		return nil, err
	}
	if err := validateColumnIntent(&intent); err != nil {
		return nil, err
	}
	payload, err := protojson.Marshal(&intent)
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

// ConvertColumn changes representations without a connection or fabricated
// sequence state. The result retains the exact source owner and requires the
// destination model's own normalizer before publication.
func ConvertColumn(value *protocol.Envelope, from, to protocol.ValueInput_Representation) (*protocol.Envelope, error) {
	if value.GetKind() != ColumnProperties {
		return nil, fmt.Errorf("YDB column conversion requires a column model")
	}
	if from == protocol.ValueInput_OBSERVED && to == protocol.ValueInput_DESIRED {
		normalized, err := Normalize(value)
		if err != nil {
			return nil, err
		}
		var observed native.ColumnMeta
		if err := protojson.Unmarshal(normalized.Payload, &observed); err != nil {
			return nil, err
		}
		intent, err := columnIntent(&observed)
		if err != nil {
			return nil, err
		}
		return convertedEnvelope(value, ColumnDesiredModel(), intent, NormalizeDesired)
	}
	if from == protocol.ValueInput_DESIRED && to == protocol.ValueInput_OBSERVED {
		normalized, err := normalizeColumnIntent(value)
		if err != nil {
			return nil, err
		}
		var intent native.ColumnIntent
		if err := protojson.Unmarshal(normalized.Payload, &intent); err != nil {
			return nil, err
		}
		return convertedEnvelope(value, model(ColumnProperties), columnObservation(&intent), Normalize)
	}
	return nil, fmt.Errorf("YDB column conversion requires distinct schema representations")
}

func columnIntent(observed *native.ColumnMeta) (*native.ColumnIntent, error) {
	result := &native.ColumnIntent{Family: observed.Family, Compression: proto.CloneOf(observed.Compression)}
	for _, encoding := range observed.Encoding {
		result.Encoding = append(result.Encoding, proto.CloneOf(encoding))
	}
	switch value := observed.DefaultValue.(type) {
	case *native.ColumnMeta_FromLiteral:
		result.DefaultValue = &native.ColumnIntent_Literal{Literal: proto.CloneOf(value.FromLiteral)}
	case *native.ColumnMeta_FromSequence:
		sequence := value.FromSequence
		if sequence.GetSetVal() != nil {
			return nil, fmt.Errorf("YDB sequence position cannot become a schema declaration")
		}
		result.DefaultValue = &native.ColumnIntent_Sequence{Sequence: &native.SequenceIntent{
			Name: sequence.Name, MinValue: sequence.MinValue, MaxValue: sequence.MaxValue, StartValue: sequence.StartValue,
			Cache: sequence.Cache, Increment: sequence.Increment, Cycle: sequence.Cycle, DataType: proto.CloneOf(sequence.DataType),
		}}
	case *native.ColumnMeta_EmptyDefault:
		result.DefaultValue = &native.ColumnIntent_DropDefault{DropDefault: true}
	}
	return result, nil
}

func columnObservation(intent *native.ColumnIntent) *native.ColumnMeta {
	result := &native.ColumnMeta{Family: intent.Family, Compression: proto.CloneOf(intent.Compression)}
	for _, encoding := range intent.Encoding {
		result.Encoding = append(result.Encoding, proto.CloneOf(encoding))
	}
	switch value := intent.DefaultValue.(type) {
	case *native.ColumnIntent_Literal:
		result.DefaultValue = &native.ColumnMeta_FromLiteral{FromLiteral: proto.CloneOf(value.Literal)}
	case *native.ColumnIntent_Sequence:
		sequence := value.Sequence
		result.DefaultValue = &native.ColumnMeta_FromSequence{FromSequence: &native.SequenceDescription{
			Name: sequence.Name, MinValue: sequence.MinValue, MaxValue: sequence.MaxValue, StartValue: sequence.StartValue,
			Cache: sequence.Cache, Increment: sequence.Increment, Cycle: sequence.Cycle, DataType: proto.CloneOf(sequence.DataType),
		}}
	case *native.ColumnIntent_DropDefault:
		result.DefaultValue = &native.ColumnMeta_EmptyDefault{}
	}
	return result
}

func validateColumnIntent(intent *native.ColumnIntent) error {
	if literal := intent.GetLiteral(); literal != nil && (literal.Type == nil || literal.Value == nil) {
		return fmt.Errorf("YDB literal default requires type and value")
	}
	if _, drop := intent.DefaultValue.(*native.ColumnIntent_DropDefault); drop && !intent.GetDropDefault() {
		return fmt.Errorf("YDB dropDefault must be true")
	}
	return nil
}
