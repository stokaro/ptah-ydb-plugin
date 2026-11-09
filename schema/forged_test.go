package schema_test

import (
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// forgedValue is a value of kind under owner in representation, desired or
// observed, whose payload is written by hand.
func forgedValue(kind string, representation protocol.ValueInput_Representation, owner *protocol.Reference, payload string) *protocol.Envelope {
	definition := schema.NativeDesiredModel(kind)
	for _, observed := range schema.ObservedModels() {
		if representation == protocol.ValueInput_OBSERVED && observed.Kind == kind {
			definition = observed
		}
	}
	return &protocol.Envelope{ProviderId: schema.ProviderID, Kind: kind, Owner: owner, SchemaVersion: definition.SchemaVersion,
		Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(payload)}
}

// normalizeIn normalizes a value of representation, desired or observed, as
// the host's NormalizeValues does.
func normalizeIn(representation protocol.ValueInput_Representation) func(*protocol.Envelope) (*protocol.Envelope, error) {
	return map[protocol.ValueInput_Representation]func(*protocol.Envelope) (*protocol.Envelope, error){
		protocol.ValueInput_DESIRED: schema.NormalizeDesired, protocol.ValueInput_OBSERVED: schema.Normalize,
	}[representation]
}

// forgedChange is a change or an operation of definition under owner whose
// payload is written by hand.
func forgedChange(definition *protocol.ValueModel, owner *protocol.Reference, payload string) *protocol.Envelope {
	return &protocol.Envelope{ProviderId: schema.ProviderID, Kind: definition.Kind, Owner: owner, SchemaVersion: definition.SchemaVersion,
		Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(payload)}
}
