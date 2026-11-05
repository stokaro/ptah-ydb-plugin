package tablecreate

import (
	"encoding/json"
	"fmt"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A table service request holds its column families as the pinned
// Ydb.Table.ColumnFamily, which carries a family's cache mode as an unknown
// field (see [schema.RequestFamily]), and protojson writes no unknown field. So
// an operation payload writes each family of a request as the provider's
// ColumnFamily, whose cacheMode is the cache mode's upstream JSON name, and
// reads it back the same way: the payload is the request as protojson writes
// the upstream message.

// familyFields are the fields of a request message that hold column families.
func familyFields(message protoreflect.MessageDescriptor) []protoreflect.FieldDescriptor {
	family := (&Ydb_Table.ColumnFamily{}).ProtoReflect().Descriptor().FullName()
	var fields []protoreflect.FieldDescriptor
	for i := range message.Fields().Len() {
		field := message.Fields().Get(i)
		if field.IsList() && field.Message() != nil && field.Message().FullName() == family {
			fields = append(fields, field)
		}
	}
	return fields
}

// marshalRequest writes request as protojson, each of its column families
// with the cache mode it carries.
func marshalRequest(request proto.Message) ([]byte, error) {
	message := proto.Clone(request).ProtoReflect()
	families := make(map[string][]json.RawMessage)
	for _, field := range familyFields(message.Descriptor()) {
		list := message.Get(field).List()
		for i := range list.Len() {
			family, err := schema.NativeFamily(list.Get(i).Message().Interface().(*Ydb_Table.ColumnFamily))
			if err != nil {
				return nil, err
			}
			encoded, err := protojson.Marshal(family)
			if err != nil {
				return nil, err
			}
			families[field.JSONName()] = append(families[field.JSONName()], encoded)
		}
		message.Clear(field)
	}
	encoded, err := protojson.Marshal(message.Interface())
	if err != nil || len(families) == 0 {
		return encoded, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	for name, list := range families {
		if fields[name], err = json.Marshal(list); err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

// unmarshalRequest reads what marshalRequest writes into request. A family
// field the provider's ColumnFamily does not declare is refused, as protojson
// refuses any other unknown field.
func unmarshalRequest(data []byte, request proto.Message) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	message := request.ProtoReflect()
	read := make(map[protoreflect.FieldNumber][]*Ydb_Table.ColumnFamily)
	for _, field := range familyFields(message.Descriptor()) {
		raw, found := fields[field.JSONName()]
		if _, named := fields[field.TextName()]; named && field.TextName() != field.JSONName() {
			return fmt.Errorf("%s: a request names its column families by their JSON name", field.TextName())
		}
		if !found {
			continue
		}
		delete(fields, field.JSONName())
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		for _, item := range list {
			var family native.ColumnFamily
			if err := protojson.Unmarshal(item, &family); err != nil {
				return err
			}
			read[field.Number()] = append(read[field.Number()], schema.RequestFamily(&family))
		}
	}
	rest, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := protojson.Unmarshal(rest, request); err != nil {
		return err
	}
	for _, field := range familyFields(message.Descriptor()) {
		list := message.Mutable(field).List()
		for _, family := range read[field.Number()] {
			list.Append(protoreflect.ValueOfMessage(family.ProtoReflect()))
		}
	}
	return nil
}
