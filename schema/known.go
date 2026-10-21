package schema

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Known refuses unknown fields and enum values recursively. Protobuf's normal
// unknown-field retention must never become silent field loss in JSON.
func Known(value proto.Message) error {
	if value == nil || !value.ProtoReflect().IsValid() {
		return fmt.Errorf("missing YDB description")
	}
	return knownMessage(value.ProtoReflect())
}

func knownMessage(value protoreflect.Message) error {
	if !value.IsValid() {
		return fmt.Errorf("missing YDB description")
	}
	if len(value.GetUnknown()) != 0 {
		return fmt.Errorf("unknown fields in %s", value.Descriptor().FullName())
	}
	var result error
	value.Range(func(field protoreflect.FieldDescriptor, data protoreflect.Value) bool {
		switch {
		case field.IsMap():
			data.Map().Range(func(_ protoreflect.MapKey, entry protoreflect.Value) bool {
				result = knownValue(field.MapValue(), entry)
				return result == nil
			})
		case field.IsList():
			list := data.List()
			for i := 0; i < list.Len(); i++ {
				if result = knownValue(field, list.Get(i)); result != nil {
					break
				}
			}
		default:
			result = knownValue(field, data)
		}
		return result == nil
	})
	return result
}

func knownValue(field protoreflect.FieldDescriptor, value protoreflect.Value) error {
	switch field.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return knownMessage(value.Message())
	case protoreflect.EnumKind:
		if field.Enum().Values().ByNumber(value.Enum()) == nil {
			return fmt.Errorf("unknown enum in %s", field.FullName())
		}
	}
	return nil
}
