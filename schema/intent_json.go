package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func convertedEnvelope(source *protocol.Envelope, definition *protocol.ValueModel, payload proto.Message, normalize func(*protocol.Envelope) (*protocol.Envelope, error)) (*protocol.Envelope, error) {
	encoded, err := protojson.Marshal(payload)
	if err != nil {
		return nil, err
	}
	value := proto.CloneOf(source)
	value.SchemaVersion, value.Encoding, value.DefinitionSha256 = definition.SchemaVersion, definition.Encoding, definition.DefinitionSha256
	value.Payload = encoded
	return normalize(value)
}

// Protojson accepts null as absence, which would erase an authored setting.
// The native NullValue enum is the exception: it is a typed literal value.
func rejectNullSettings(raw []byte, descriptor protoreflect.MessageDescriptor) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	} // Protojson validates non-object well-known types.
	for name, raw := range fields {
		field := descriptor.Fields().ByJSONName(name)
		if field == nil {
			field = descriptor.Fields().ByName(protoreflect.Name(name))
		}
		if field == nil {
			continue
		} // Protojson refuses unknown fields.
		if bytes.Equal(raw, []byte("null")) {
			if field.Enum() != nil && field.Enum().FullName() == "google.protobuf.NullValue" {
				continue
			}
			return fmt.Errorf("null YDB model setting")
		}
		if field.IsMap() {
			var entries map[string]json.RawMessage
			if err := json.Unmarshal(raw, &entries); err != nil {
				return err
			}
			for _, entry := range entries {
				if bytes.Equal(entry, []byte("null")) {
					return fmt.Errorf("null YDB model map value")
				}
				if field.MapValue().Message() != nil {
					if err := rejectNullSettings(entry, field.MapValue().Message()); err != nil {
						return err
					}
				}
			}
		} else if field.IsList() {
			var items []json.RawMessage
			if err := json.Unmarshal(raw, &items); err != nil {
				return err
			}
			for _, item := range items {
				if bytes.Equal(item, []byte("null")) {
					return fmt.Errorf("null YDB model list item")
				}
				if field.Message() != nil {
					if err := rejectNullSettings(item, field.Message()); err != nil {
						return err
					}
				}
			}
		} else if field.Message() != nil {
			if err := rejectNullSettings(raw, field.Message()); err != nil {
				return err
			}
		}
	}
	return nil
}
