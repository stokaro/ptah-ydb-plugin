package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func indexObservation(name string) *native.ObservedIndex {
	return &native.ObservedIndex{
		Description: &native.TableIndexDescription{Name: name, IndexColumns: []string{"label"}, DataColumns: []string{"extra"}, Status: native.TableIndexDescription_STATUS_READY, Type: &native.TableIndexDescription_GlobalIndex{GlobalIndex: &native.GlobalIndex{}}},
		Tables: []*native.ImplementationTable{{Name: "indexImplTable", Description: &native.DescribeTableResult{
			Columns: []*native.ColumnMeta{{Name: "label", Type: primitive(Ydb.Type_UTF8)}, {Name: "id", Type: primitive(Ydb.Type_UINT64)}}, PrimaryKey: []string{"label", "id"},
			PartitioningSettings: &Ydb_Table.PartitioningSettings{MinPartitionsCount: 4},
		}}},
	}
}

func TestIndexObservationsCanonicalizeOnlyUnorderedLists(t *testing.T) {
	c := qt.New(t)
	input := &native.IndexObservations{Indexes: []*native.ObservedIndex{indexObservation("z"), indexObservation("a")}}
	input.Indexes[0].Description.SizeBytes = 200
	input.Indexes[0].Tables[0].Description.TableStats = &Ydb_Table.TableStats{}
	captured, err := schema.CaptureIndexes(tableOwner(), input)
	c.Assert(err, qt.IsNil)
	var decoded native.IndexObservations
	c.Assert(protojson.Unmarshal(captured.Payload, &decoded), qt.IsNil)
	c.Assert(decoded.Indexes[0].Description.Name, qt.Equals, "a")
	c.Assert(decoded.Indexes[1].Description.Name, qt.Equals, "z")
	c.Assert(decoded.Indexes[1].Description.SizeBytes, qt.Equals, uint64(0))
	c.Assert(decoded.Indexes[1].Tables[0].Description.TableStats, qt.IsNil)
	c.Assert(decoded.Indexes[1].Tables[0].Description.PrimaryKey, qt.DeepEquals, []string{"label", "id"})
	c.Assert(decoded.Indexes[1].Tables[0].Description.GetPartitioningSettings().GetMinPartitionsCount(), qt.Equals, uint64(4))
	c.Assert(input.Indexes[0].Description.Name, qt.Equals, "z")
	c.Assert(input.Indexes[0].Description.SizeBytes, qt.Equals, uint64(200))
	normalized, err := schema.Normalize(captured)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized, captured), qt.IsTrue)
}

func TestIndexObservationsRefuseIncompleteOrUnknownState(t *testing.T) {
	c := qt.New(t)
	base, err := schema.CaptureIndexes(tableOwner(), &native.IndexObservations{})
	c.Assert(err, qt.IsNil)
	for _, test := range []struct{ name, payload string }{
		{"missing implementation", `{"indexes":[{"description":{"name":"idx","indexColumns":["v"],"status":"STATUS_READY","globalIndex":{}}}]}`},
		{"not ready", `{"indexes":[{"description":{"name":"idx","indexColumns":["v"],"status":"STATUS_BUILDING","globalIndex":{}}}]}`},
		{"unknown kind", `{"indexes":[{"description":{"name":"idx","indexColumns":["v"],"status":"STATUS_READY","localIndex":{}}}]}`},
		{"missing kind", `{"indexes":[{"description":{"name":"idx","indexColumns":["v"],"status":"STATUS_READY"}}]}`},
		{"path escape", `{"indexes":[{"description":{"name":"../idx","indexColumns":["v"],"status":"STATUS_READY","globalIndex":{}}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value := proto.CloneOf(base)
			value.Payload = []byte(test.payload)
			result, err := schema.Normalize(value)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
	missing := indexObservation("idx")
	missing.Tables[0].Description = nil
	duplicate := indexObservation("idx")
	duplicate.Tables = append(duplicate.Tables, proto.CloneOf(duplicate.Tables[0]))
	unknown := indexObservation("idx")
	unknown.Tables[0].Description.Columns[0].ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 400, protowire.VarintType), 1))
	for _, test := range []struct {
		name  string
		value *native.IndexObservations
	}{
		{"missing table description", &native.IndexObservations{Indexes: []*native.ObservedIndex{missing}}},
		{"duplicate implementation", &native.IndexObservations{Indexes: []*native.ObservedIndex{duplicate}}},
		{"duplicate indexes", &native.IndexObservations{Indexes: []*native.ObservedIndex{indexObservation("idx"), indexObservation("idx")}}},
		{"unknown physical column", &native.IndexObservations{Indexes: []*native.ObservedIndex{unknown}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := schema.CaptureIndexes(tableOwner(), test.value)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

// Field 9 is returned by YDB for the vector-prefix implementation sequence,
// but the upstream Go bindings cannot decode it. Keep it typed in our model.
func TestNativeSequenceTypeSurvivesWireAndOfflineNormalization(t *testing.T) {
	c := qt.New(t)
	dataType, err := proto.Marshal(primitive(Ydb.Type_INT64))
	c.Assert(err, qt.IsNil)
	sequence := &Ydb_Table.SequenceDescription{Name: proto.String("_serial"), Increment: proto.Int64(1)}
	sequence.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, 9, protowire.BytesType), dataType))
	upstream := &Ydb_Table.ColumnMeta{Name: "id", Type: primitive(Ydb.Type_INT64), DefaultValue: &Ydb_Table.ColumnMeta_FromSequence{FromSequence: sequence}}
	wire, err := proto.Marshal(upstream)
	c.Assert(err, qt.IsNil)
	var nativeColumn native.ColumnMeta
	c.Assert(proto.Unmarshal(wire, &nativeColumn), qt.IsNil)
	c.Assert(schema.Known(&nativeColumn), qt.IsNil)
	c.Assert(nativeColumn.GetFromSequence().GetDataType().GetTypeId(), qt.Equals, Ydb.Type_INT64)
	owner := tableOwner()
	owner.Kind = "column"
	owner.Path = []string{owner.Name}
	owner.Name = "id"
	captured, err := schema.CaptureColumn(owner, &nativeColumn)
	c.Assert(err, qt.IsNil)
	var decoded native.ColumnMeta
	c.Assert(protojson.Unmarshal(captured.Payload, &decoded), qt.IsNil)
	c.Assert(proto.Equal(decoded.GetFromSequence(), nativeColumn.GetFromSequence()), qt.IsTrue)
	c.Assert(decoded.Type, qt.IsNil)
}
