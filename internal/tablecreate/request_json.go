package tablecreate

import (
	"encoding/json"
	"fmt"
	"slices"

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

// A creation request holds its vector and full-text indexes as the pinned
// Ydb.Table.TableIndex, which carries each one's arm as an unknown field (see
// [schema.RequestIndex]). So the payload writes such an index as the
// provider's IndexIntent, whose fields keep the upstream index's JSON names,
// and reads an index that names a vector or full-text arm the same way.

// nativeIndexKeys are the JSON names of the vector and full-text arms of an
// index, in both spellings protojson reads.
var nativeIndexKeys = []string{"globalVectorKmeansTreeIndex", "global_vector_kmeans_tree_index", "globalFulltextPlainIndex", "global_fulltext_plain_index",
	"globalFulltextRelevanceIndex", "global_fulltext_relevance_index"}

// indexFields are the fields of a request message that hold indexes.
func indexFields(message protoreflect.MessageDescriptor) []protoreflect.FieldDescriptor {
	index := (&Ydb_Table.TableIndex{}).ProtoReflect().Descriptor().FullName()
	var fields []protoreflect.FieldDescriptor
	for i := range message.Fields().Len() {
		field := message.Fields().Get(i)
		if field.IsList() && field.Message() != nil && field.Message().FullName() == index {
			fields = append(fields, field)
		}
	}
	return fields
}

// marshalIndex writes one index of a request: a vector or full-text one as
// the provider's IndexIntent, any other as protojson writes the upstream
// message.
func marshalIndex(index *Ydb_Table.TableIndex) (json.RawMessage, error) {
	spec, err := schema.NativeRequestIndex(index)
	if err != nil {
		return nil, err
	}
	if spec != nil {
		return protojson.Marshal(spec)
	}
	return protojson.Marshal(index)
}

// unmarshalIndex reads what marshalIndex writes.
func unmarshalIndex(data []byte) (*Ydb_Table.TableIndex, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(nativeIndexKeys, func(key string) bool { _, found := keys[key]; return found }) {
		var index Ydb_Table.TableIndex
		if err := protojson.Unmarshal(data, &index); err != nil {
			return nil, err
		}
		return &index, nil
	}
	var spec native.IndexIntent
	if err := protojson.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	return schema.RequestIndex(&spec)
}

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
// with the cache mode it carries and each of its indexes with the vector or
// full-text arm it carries.
func marshalRequest(request proto.Message) ([]byte, error) {
	message := proto.Clone(request).ProtoReflect()
	lists := make(map[string][]json.RawMessage)
	for _, field := range indexFields(message.Descriptor()) {
		list := message.Get(field).List()
		for i := range list.Len() {
			encoded, err := marshalIndex(list.Get(i).Message().Interface().(*Ydb_Table.TableIndex))
			if err != nil {
				return nil, err
			}
			lists[field.JSONName()] = append(lists[field.JSONName()], encoded)
		}
		message.Clear(field)
	}
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
			lists[field.JSONName()] = append(lists[field.JSONName()], encoded)
		}
		message.Clear(field)
	}
	encoded, err := protojson.Marshal(message.Interface())
	if err != nil || len(lists) == 0 {
		return encoded, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	for name, list := range lists {
		if fields[name], err = json.Marshal(list); err != nil {
			return nil, err
		}
	}
	return json.Marshal(fields)
}

// unmarshalRequest reads what marshalRequest writes into request. A family
// field the provider's ColumnFamily does not declare, and an index field the
// provider's IndexIntent does not declare, are refused, as protojson refuses
// any other unknown field.
func unmarshalRequest(data []byte, request proto.Message) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	message := request.ProtoReflect()
	indexes, err := unmarshalIndexes(fields, message.Descriptor())
	if err != nil {
		return err
	}
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
	for _, field := range indexFields(message.Descriptor()) {
		list := message.Mutable(field).List()
		for _, index := range indexes[field.Number()] {
			list.Append(protoreflect.ValueOfMessage(index.ProtoReflect()))
		}
	}
	return nil
}

// unmarshalIndexes reads the index fields of message out of fields, the
// request's JSON object, and removes them from it.
func unmarshalIndexes(fields map[string]json.RawMessage, message protoreflect.MessageDescriptor) (map[protoreflect.FieldNumber][]*Ydb_Table.TableIndex, error) {
	read := make(map[protoreflect.FieldNumber][]*Ydb_Table.TableIndex)
	for _, field := range indexFields(message) {
		raw, found := fields[field.JSONName()]
		if _, named := fields[field.TextName()]; named && field.TextName() != field.JSONName() {
			return nil, fmt.Errorf("%s: a request names its indexes by their JSON name", field.TextName())
		}
		if !found {
			continue
		}
		delete(fields, field.JSONName())
		var list []json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, err
		}
		for _, item := range list {
			index, err := unmarshalIndex(item)
			if err != nil {
				return nil, err
			}
			read[field.Number()] = append(read[field.Number()], index)
		}
	}
	return read, nil
}
