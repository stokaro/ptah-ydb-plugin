package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
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
	c := testcheck.New(t)
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
	c := testcheck.New(t)
	base, err := schema.CaptureIndexes(tableOwner(), &native.IndexObservations{})
	c.Assert(err, qt.IsNil)
	for _, test := range []struct{ name, payload string }{
		{"missing implementation", `{"indexes":[{"description":{"name":"idx","indexColumns":["v"],"status":"STATUS_READY","globalIndex":{}}}]}`},
		{"not ready", `{"indexes":[{"description":{"name":"idx","indexColumns":["v"],"status":"STATUS_BUILDING","globalIndex":{}}}]}`},
		{"unknown kind", `{"indexes":[{"description":{"name":"idx","indexColumns":["v"],"status":"STATUS_READY","localIndex":{}}}]}`},
		{"missing kind", `{"indexes":[{"description":{"name":"idx","indexColumns":["v"],"status":"STATUS_READY"}}]}`},
		{"path escape", `{"indexes":[{"description":{"name":"../idx","indexColumns":["v"],"status":"STATUS_READY","globalIndex":{}}}]}`},
		{"a common index of a native kind", `{"commonIndexes":[{"name":"idx","indexColumns":["v"],"status":"STATUS_READY","globalVectorKmeansTreeIndex":{}}]}`},
		{"a common index with no column", `{"commonIndexes":[{"name":"idx","status":"STATUS_READY","globalIndex":{}}]}`},
		{"a common index with statistics", `{"commonIndexes":[{"name":"idx","indexColumns":["v"],"globalIndex":{},"sizeBytes":"7"}]}`},
		{"a common index the native set holds too", `{"indexes":[{"description":{"name":"idx","indexColumns":["v"],"status":"STATUS_READY","globalIndex":{}},` +
			`"tables":[{"name":"indexImplTable","description":{"columns":[{"name":"v","type":{"typeId":"UTF8"}}],"primaryKey":["v"]}}]}],` +
			`"commonIndexes":[{"name":"idx","indexColumns":["v"],"globalIndex":{}}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
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
			c := testcheck.New(t)
			result, err := schema.CaptureIndexes(tableOwner(), test.value)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}

// A read that leaves the indexes the common model describes out of the native
// set describes each beside it, for the partitioning and read replicas the
// common model has no place for. The capture keeps those settings, drops the
// size, orders the descriptions by name, and reads no status, which the
// common model owns.
func TestIndexObservationsKeepTheSettingsOfCommonIndexes(t *testing.T) {
	c := testcheck.New(t)
	settings := &native.GlobalIndexSettings{PartitioningSettings: &Ydb_Table.PartitioningSettings{PartitioningByLoad: Ydb.FeatureFlag_ENABLED, MinPartitionsCount: 2}}
	input := &native.IndexObservations{CommonIndexes: []*native.TableIndexDescription{
		{Name: "z", IndexColumns: []string{"label"}, Status: native.TableIndexDescription_STATUS_BUILDING, SizeBytes: 300,
			Type: &native.TableIndexDescription_GlobalAsyncIndex{GlobalAsyncIndex: &native.GlobalAsyncIndex{Settings: settings}}},
		{Name: "a", IndexColumns: []string{"label"}, Type: &native.TableIndexDescription_GlobalUniqueIndex{GlobalUniqueIndex: &native.GlobalUniqueIndex{}}},
	}}
	captured, err := schema.CaptureIndexes(tableOwner(), input)
	c.Assert(err, qt.IsNil)
	var decoded native.IndexObservations
	c.Assert(protojson.Unmarshal(captured.Payload, &decoded), qt.IsNil)
	c.Assert(decoded.Indexes, qt.HasLen, 0)
	c.Assert(decoded.CommonIndexes, qt.HasLen, 2)
	c.Assert(decoded.CommonIndexes[0].Name, qt.Equals, "a")
	c.Assert(decoded.CommonIndexes[1].SizeBytes, qt.Equals, uint64(0))
	c.Assert(proto.Equal(decoded.CommonIndexes[1].GetGlobalAsyncIndex().GetSettings(), settings), qt.IsTrue)
	c.Assert(input.CommonIndexes[0].SizeBytes, qt.Equals, uint64(300))
	normalized, err := schema.Normalize(captured)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(normalized, captured), qt.IsTrue)
	// A declaration of the native index set states no common index, so the
	// declaration a read converts to holds none of the descriptions.
	declared, err := schema.ConvertNative(captured, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	c.Assert(string(declared.Payload), qt.Equals, `{}`)
}

// Field 9 is returned by YDB for the vector-prefix implementation sequence,
// but the upstream Go bindings cannot decode it. Keep it typed in our model.
func TestNativeSequenceTypeSurvivesWireAndOfflineNormalization(t *testing.T) {
	c := testcheck.New(t)
	dataType, err := proto.Marshal(primitive(Ydb.Type_INT64))
	c.Assert(err, qt.IsNil)
	sequence := &Ydb_Table.SequenceDescription{Name: new("_serial"), Increment: proto.Int64(1)}
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

// An index of a kind the provider does not read is refused by its name and its
// kind, rather than as a description that does not decode: a kind YDB carries
// in a field the provider's model does not hold. Ported from the host's
// TestReader_UnsupportedIndexDiagnosticNamesTheKind.
func TestValidateIndexesNamesAKindItDoesNotRead_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		field protowire.Number
		kind  string
	}{
		{name: "bloom", field: 12, kind: "bloom_filter index"},
		{name: "ngram", field: 13, kind: "bloom_ngram_filter index"},
		{name: "JSON", field: 14, kind: "global JSON index"},
		{name: "min-max", field: 15, kind: "min_max index"},
		{name: "unknown", field: 999, kind: "index of a kind this provider does not know"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			index := &native.TableIndexDescription{Name: "body_idx", IndexColumns: []string{"body"}, Status: native.TableIndexDescription_STATUS_READY}
			index.ProtoReflect().SetUnknown(protowire.AppendBytes(protowire.AppendTag(nil, test.field, protowire.BytesType), nil))

			err := schema.ValidateIndexes([]*native.TableIndexDescription{index})

			c.Assert(err, qt.ErrorMatches, `index "body_idx" is a `+test.kind+`, which this provider does not read`)
		})
	}
}
