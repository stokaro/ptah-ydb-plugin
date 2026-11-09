package schema

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// WithoutAccess is a desired value converted from a read with its native
// access requirement left out, for a target that carries grants on the
// common wire: the grants travel as common grants and would repeat it, and
// a creation cannot establish an owner or effective permissions. A value of
// a kind without an access requirement is returned as it is.
func WithoutAccess(value *protocol.Envelope) (*protocol.Envelope, error) {
	switch value.GetKind() {
	case TableProperties:
		normalized, err := normalizeTableIntent(value)
		if err != nil {
			return nil, err
		}
		return withoutAccessField(value, normalized, &native.RowTableIntent{}, TableDesiredModel(), NormalizeDesired)
	case CoordinationProperties:
		intent, err := CoordinationConfiguration(value, protocol.ValueInput_DESIRED)
		if err != nil {
			return nil, err
		}
		if intent.Access == nil {
			return value, nil
		}
		intent.Access = nil
		converted, err := CaptureCoordinationIntent(value.Owner, intent)
		if err != nil {
			return nil, err
		}
		converted.Context = proto.CloneOf(value.Context)
		return converted, nil
	}
	target := nativeIntentMessage(value.GetKind())
	if target == nil {
		return value, nil
	}
	normalized, err := normalizeNativeIntent(value)
	if err != nil {
		return nil, err
	}
	return withoutAccessField(value, normalized, target, NativeDesiredModel(value.GetKind()), NormalizeDesired)
}

func withoutAccessField(value, normalized *protocol.Envelope, target proto.Message, definition *protocol.ValueModel,
	normalize func(*protocol.Envelope) (*protocol.Envelope, error)) (*protocol.Envelope, error) {
	field := target.ProtoReflect().Descriptor().Fields().ByName("access")
	if field == nil {
		return value, nil
	}
	if err := protojson.Unmarshal(normalized.Payload, target); err != nil {
		return nil, fmt.Errorf("decode the desired YDB value: %w", err)
	}
	if !target.ProtoReflect().Has(field) {
		return value, nil
	}
	target.ProtoReflect().Clear(field)
	return convertedEnvelope(value, definition, target, normalize)
}
