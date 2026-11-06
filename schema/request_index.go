package schema

import (
	"fmt"
	"slices"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// The wire numbers of the vector and full-text arms of Ydb.Table.TableIndex in
// the upstream table protocol (ydb_table.proto at the commit
// third_party/ydb/NOTICE.md names). The pinned Go bindings declare none of
// them and keep each as an unknown field of the index.
const (
	requestVectorIndexField            protowire.Number = 7
	requestFulltextPlainIndexField     protowire.Number = 8
	requestFulltextRelevanceIndexField protowire.Number = 9
)

// RequestIndex returns spec, a vector or full-text index, as a table service
// creation request takes it: the pinned Ydb.Table.TableIndex with its name,
// key and covered columns, and the index's upstream arm, which the bindings
// keep as an unknown field and send with the request. Measured on 25.1.4.7
// and 26.2.1.14, CreateTable builds such an index with the table, as YQL's
// CREATE TABLE with the index's INDEX clause does. Any other kind is refused.
func RequestIndex(spec *native.IndexIntent) (*Ydb_Table.TableIndex, error) {
	var number protowire.Number
	var arm proto.Message
	switch kind := spec.GetType().(type) {
	case *native.IndexIntent_GlobalVectorKmeansTreeIndex:
		number, arm = requestVectorIndexField, kind.GlobalVectorKmeansTreeIndex
	case *native.IndexIntent_GlobalFulltextPlainIndex:
		number, arm = requestFulltextPlainIndexField, kind.GlobalFulltextPlainIndex
	case *native.IndexIntent_GlobalFulltextRelevanceIndex:
		number, arm = requestFulltextRelevanceIndexField, kind.GlobalFulltextRelevanceIndex
	default:
		return nil, fmt.Errorf("YDB index %q is no vector or full-text index", spec.GetName())
	}
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(arm)
	if err != nil {
		return nil, err
	}
	index := &Ydb_Table.TableIndex{Name: spec.GetName(), IndexColumns: slices.Clone(spec.GetIndexColumns()), DataColumns: slices.Clone(spec.GetDataColumns())}
	index.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, number, protowire.BytesType), body))
	return index, nil
}

// NativeRequestIndex returns the vector or full-text index a creation
// request's index carries (see [RequestIndex]), or nil for an index with no
// unknown field. An index whose unknown fields hold anything but one such arm,
// or hold one beside a kind the bindings name, is refused.
func NativeRequestIndex(index *Ydb_Table.TableIndex) (*native.IndexIntent, error) {
	unknown := index.ProtoReflect().GetUnknown()
	if len(unknown) == 0 {
		return nil, nil
	}
	refused := fmt.Errorf("unknown fields in %s", index.ProtoReflect().Descriptor().FullName())
	number, wireType, length := protowire.ConsumeTag(unknown)
	if length < 0 || wireType != protowire.BytesType || index.GetType() != nil {
		return nil, refused
	}
	body, bodyLength := protowire.ConsumeBytes(unknown[length:])
	if bodyLength < 0 || length+bodyLength != len(unknown) {
		return nil, refused
	}
	result := &native.IndexIntent{Name: index.GetName(), IndexColumns: slices.Clone(index.GetIndexColumns()), DataColumns: slices.Clone(index.GetDataColumns())}
	var arm proto.Message
	switch number {
	case requestVectorIndexField:
		vector := &native.GlobalVectorKMeansTreeIndex{}
		arm, result.Type = vector, &native.IndexIntent_GlobalVectorKmeansTreeIndex{GlobalVectorKmeansTreeIndex: vector}
	case requestFulltextPlainIndexField:
		plain := &native.GlobalFulltextPlainIndex{}
		arm, result.Type = plain, &native.IndexIntent_GlobalFulltextPlainIndex{GlobalFulltextPlainIndex: plain}
	case requestFulltextRelevanceIndexField:
		relevance := &native.GlobalFulltextRelevanceIndex{}
		arm, result.Type = relevance, &native.IndexIntent_GlobalFulltextRelevanceIndex{GlobalFulltextRelevanceIndex: relevance}
	default:
		return nil, refused
	}
	if err := proto.Unmarshal(body, arm); err != nil {
		return nil, refused
	}
	if err := Known(arm); err != nil {
		return nil, err
	}
	return result, nil
}

// requestIndexArm reports unknown fields of value that are only the vector or
// full-text arm of a request's index (see [RequestIndex]).
func requestIndexArm(value proto.Message) bool {
	index, ok := value.(*Ydb_Table.TableIndex)
	if !ok {
		return false
	}
	spec, err := NativeRequestIndex(index)
	return err == nil && spec != nil
}
