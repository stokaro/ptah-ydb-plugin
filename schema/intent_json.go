package schema

import (
	"bytes"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	protocol "ptah.run/provider-sdk/protocol/experimental"
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
		if err := rejectNullField(field, raw); err != nil {
			return err
		}
	}
	return nil
}

// rejectNullField refuses a null in the value of one field: the value itself,
// a map value or a list item, and a null setting of a message the field holds.
// A null of the native NullValue enum is a typed literal and passes.
func rejectNullField(field protoreflect.FieldDescriptor, raw json.RawMessage) error {
	if bytes.Equal(raw, []byte("null")) {
		if field.Enum() != nil && field.Enum().FullName() == "google.protobuf.NullValue" {
			return nil
		}
		return fmt.Errorf("null YDB model setting")
	}
	switch {
	case field.IsMap():
		return rejectNullMapValues(field, raw)
	case field.IsList():
		return rejectNullListItems(field, raw)
	case field.Message() != nil:
		return rejectNullSettings(raw, field.Message())
	}
	return nil
}

// rejectNullMapValues refuses a null value of a map field and checks each
// message value for null settings.
func rejectNullMapValues(field protoreflect.FieldDescriptor, raw json.RawMessage) error {
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
	return nil
}

// rejectNullListItems refuses a null item of a list field and checks each
// message item for null settings.
func rejectNullListItems(field protoreflect.FieldDescriptor, raw json.RawMessage) error {
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
	return nil
}
