package schema

import (
	"fmt"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Known refuses unknown fields and enum values recursively. Protobuf's normal
// unknown-field retention must never become silent field loss in JSON.
//
// The unknown fields it takes are the ones the pinned bindings lack and a
// table service request carries, which the provider writes and reads itself:
// the cache mode of a request's column family, field 6 of
// Ydb.Table.ColumnFamily, holding a mode the provider's ColumnFamily names
// (see [RequestFamily]), and the vector or full-text arm of a creation
// request's index, field 7, 8 or 9 of Ydb.Table.TableIndex, holding a known
// arm (see [RequestIndex]).
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
	if unknown := value.GetUnknown(); len(unknown) != 0 && !requestFamilyCacheMode(value, unknown) && !requestIndexArm(value.Interface()) {
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
			// STREAMING_QUERY has wire value 26 on YDB 26.2. The pinned
			// vendor binding omits it; inspection handles this exact value.
			if field.FullName() == "Ydb.Scheme.Entry.type" && value.Enum() == 26 {
				return nil
			}
			return fmt.Errorf("unknown enum in %s", field.FullName())
		}
	}
	return nil
}

// requestFamilyCacheMode reports unknown fields of value that are only the
// cache mode of a request's column family.
func requestFamilyCacheMode(value protoreflect.Message, unknown []byte) bool {
	if value.Descriptor().FullName() != (&Ydb_Table.ColumnFamily{}).ProtoReflect().Descriptor().FullName() {
		return false
	}
	_, read := requestCacheMode(unknown)
	return read
}
