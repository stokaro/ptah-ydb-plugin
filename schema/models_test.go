package schema_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func tableOwner() *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "app", Kind: "table", Name: "records"}
}

func TestTablePropertiesSeparateSharedAndTransientFields(t *testing.T) {
	c := qt.New(t)
	original := &nativeprotocol.DescribeTableResult{
		Self:    &Ydb_Scheme.Entry{Name: "records", Type: Ydb_Scheme.Entry_TABLE, Owner: "alice", SizeBytes: 100, CreatedAt: &Ydb.VirtualTimestamp{PlanStep: 3}},
		Columns: []*nativeprotocol.ColumnMeta{{Name: "a", Type: primitive(Ydb.Type_UINT64)}}, PrimaryKey: []string{"b", "a"},
		Attributes: map[string]string{"comment": "caption"}, TableStats: &Ydb_Table.TableStats{},
	}
	captured, err := schema.CaptureTable(tableOwner(), original)
	c.Assert(err, qt.IsNil)
	var properties nativeprotocol.DescribeTableResult
	c.Assert(protojson.Unmarshal(captured.Payload, &properties), qt.IsNil)
	c.Assert(properties.GetPrimaryKey(), qt.DeepEquals, []string{"b", "a"})
	c.Assert(properties.GetAttributes(), qt.DeepEquals, map[string]string{"comment": "caption"})
	c.Assert(properties.GetSelf().GetOwner(), qt.Equals, "alice")
	c.Assert(properties.GetSelf().GetName(), qt.Equals, "")
	c.Assert(properties.GetSelf().GetSizeBytes(), qt.Equals, uint64(0))
	c.Assert(properties.GetColumns(), qt.HasLen, 0)
	c.Assert(properties.GetTableStats(), qt.IsNil)
	c.Assert(original.GetSelf().GetName(), qt.Equals, "records")
	c.Assert(original.GetColumns(), qt.HasLen, 1)
	normalized, err := schema.Normalize(captured)
	c.Assert(err, qt.IsNil)
	c.Assert(string(normalized.Payload), qt.Equals, string(captured.Payload))
}

func TestColumnPropertiesRetainFamilyAndLiteral(t *testing.T) {
	c := qt.New(t)
	original := &nativeprotocol.ColumnMeta{Name: "label", Type: optional(primitive(Ydb.Type_UTF8)), Family: "cold",
		DefaultValue: &nativeprotocol.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{Type: primitive(Ydb.Type_UTF8), Value: &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: "hello"}}}},
	}
	owner := tableOwner()
	owner.Kind = "column"
	owner.Path = []string{owner.Name}
	owner.Name = "label"
	captured, err := schema.CaptureColumn(owner, original)
	c.Assert(err, qt.IsNil)
	var properties nativeprotocol.ColumnMeta
	c.Assert(protojson.Unmarshal(captured.Payload, &properties), qt.IsNil)
	c.Assert(properties.GetFamily(), qt.Equals, "cold")
	c.Assert(properties.GetFromLiteral().GetValue().GetTextValue(), qt.Equals, "hello")
	c.Assert(properties.GetName(), qt.Equals, "")
	c.Assert(properties.GetType(), qt.IsNil)
	c.Assert(original.GetType().GetOptionalType(), qt.IsNotNil)
}

func TestObservationRefusesUnknownAndDuplicatedState(t *testing.T) {
	c := qt.New(t)
	base, err := schema.CaptureTable(tableOwner(), &nativeprotocol.DescribeTableResult{PrimaryKey: []string{"id"}})
	c.Assert(err, qt.IsNil)
	for _, test := range []struct{ name, payload string }{
		{"unknown field", `{"primaryKey":["id"],"surprise":true}`},
		{"duplicate field", `{"primaryKey":["id"],"primaryKey":["other"]}`},
		{"common columns", `{"primaryKey":["id"],"columns":[{"name":"id"}]}`},
		{"statistics", `{"primaryKey":["id"],"tableStats":{}}`},
		{"self name", `{"primaryKey":["id"],"self":{"name":"other"}}`},
		{"missing primary key", `{}`},
		{"duplicate key", `{"primaryKey":["id","id"]}`},
		{"unknown enum", `{"primaryKey":["id"],"keyBloomFilter":9999}`},
		{"column store", `{"primaryKey":["id"],"storeType":"STORE_TYPE_COLUMN"}`},
		{"incomplete index", `{"primaryKey":["id"],"indexes":[{"name":"idx"}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			input := proto.CloneOf(base)
			input.Payload = []byte(test.payload)
			result, err := schema.Normalize(input)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
	wrong := proto.CloneOf(base)
	wrong.DefinitionSha256 = "wrong"
	_, err = schema.Normalize(wrong)
	c.Assert(err, qt.IsNotNil)
	wrong = proto.CloneOf(base)
	wrong.Owner.Engine = "postgres"
	_, err = schema.Normalize(wrong)
	c.Assert(err, qt.IsNotNil)
}

func TestUnknownNativeFieldsCannotDisappearDuringProjection(t *testing.T) {
	c := qt.New(t)
	value := &nativeprotocol.DescribeTableResult{PrimaryKey: []string{"id"}, Columns: []*nativeprotocol.ColumnMeta{{Name: "id", Type: primitive(Ydb.Type_UINT64)}}}
	value.Columns[0].ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 400, protowire.VarintType), 1))
	result, err := schema.CaptureTable(tableOwner(), value)
	c.Assert(result, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "unknown fields in ptah.ydb.native.ColumnMeta")
}

func TestModelKindsUseProviderNamespace(t *testing.T) {
	c := qt.New(t)
	for _, model := range schema.Models() {
		c.Assert(strings.HasPrefix(model.GetKind(), "github.com/stokaro/ptah-ydb-plugin/"), qt.IsTrue, qt.Commentf("%s", model.GetKind()))
	}
	owner := tableOwner()
	captured, err := schema.CaptureTable(owner, &nativeprotocol.DescribeTableResult{PrimaryKey: []string{"id"}})
	c.Assert(err, qt.IsNil)
	captured.Kind = "ydb.row-table-properties"
	_, err = schema.Normalize(captured)
	c.Assert(err, qt.IsNotNil)
}

func TestLiveCaptureRequiresDatabaseBinding(t *testing.T) {
	c := qt.New(t)
	owner := tableOwner()
	owner.Database = ""
	value, err := schema.CaptureTable(owner, &nativeprotocol.DescribeTableResult{PrimaryKey: []string{"id"}})
	c.Assert(value, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "captured YDB observation requires a database")
}
