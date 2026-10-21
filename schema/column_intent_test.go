package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func columnOwner() *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "Case.App", Kind: "column", Path: []string{"Orders"}, Name: "SerialID"}
}

func TestColumnConversionRetainsNativeSettings(t *testing.T) {
	for _, test := range []struct {
		name   string
		column *native.ColumnMeta
	}{
		{"omitted default", &native.ColumnMeta{Family: "cold"}},
		{"explicit no default", &native.ColumnMeta{DefaultValue: &native.ColumnMeta_EmptyDefault{}}},
		{"typed literal", &native.ColumnMeta{DefaultValue: &native.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{Type: primitive(Ydb.Type_UTF8), Value: &Ydb.Value{Value: &Ydb.Value_TextValue{TextValue: "Case.Value"}}}}}},
		{"typed null literal", &native.ColumnMeta{DefaultValue: &native.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{Type: optional(primitive(Ydb.Type_UTF8)), Value: &Ydb.Value{Value: &Ydb.Value_NullFlagValue{NullFlagValue: structpb.NullValue_NULL_VALUE}}}}}},
		{"sequence configuration", &native.ColumnMeta{DefaultValue: &native.ColumnMeta_FromSequence{FromSequence: &native.SequenceDescription{
			Name: proto.String("_serial"), MinValue: proto.Int64(-100), MaxValue: proto.Int64(100), StartValue: proto.Int64(-3),
			Cache: proto.Uint64(4), Increment: proto.Int64(2), Cycle: proto.Bool(false), DataType: primitive(Ydb.Type_INT64),
		}}}},
		{"compression and ordered encoding", &native.ColumnMeta{Compression: &native.ColumnCompression{Algorithm: native.ColumnCompression_ALGORITHM_ZSTD.Enum(), CompressionLevel: proto.Int32(0)},
			Encoding: []*native.ColumnEncoding{{EncodingSettings: &native.ColumnEncoding_Dictionary_{Dictionary: &native.ColumnEncoding_Dictionary{}}}, {EncodingSettings: &native.ColumnEncoding_Off_{Off: &native.ColumnEncoding_Off{}}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			observed, err := schema.CaptureColumn(columnOwner(), test.column)
			c.Assert(err, qt.IsNil)
			original := proto.CloneOf(observed)
			desired, err := schema.ConvertColumn(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			c.Assert(desired.DefinitionSha256, qt.Equals, schema.ColumnDesiredModel().DefinitionSha256)
			c.Assert(desired.DefinitionSha256 == observed.DefinitionSha256, qt.IsFalse)
			c.Assert(proto.Equal(desired.Owner, observed.Owner), qt.IsTrue)
			back, err := schema.ConvertColumn(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
			c.Assert(err, qt.IsNil)
			c.Assert(proto.Equal(back, observed), qt.IsTrue)
			c.Assert(proto.Equal(observed, original), qt.IsTrue)
			var intent native.ColumnIntent
			c.Assert(protojson.Unmarshal(desired.Payload, &intent), qt.IsNil)
			c.Assert(intent.Family, qt.Equals, test.column.Family)
		})
	}
}

func TestColumnConversionRefusesMutableSequencePosition(t *testing.T) {
	c := qt.New(t)
	observed, err := schema.CaptureColumn(columnOwner(), &native.ColumnMeta{DefaultValue: &native.ColumnMeta_FromSequence{FromSequence: &native.SequenceDescription{SetVal: &native.SequenceDescription_SetVal{NextValue: proto.Int64(7)}}}})
	c.Assert(err, qt.IsNil)
	result, err := schema.ConvertColumn(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(result, qt.IsNil)
	c.Assert(err, qt.ErrorMatches, "YDB sequence position cannot become a schema declaration")
}

func TestDesiredColumnRefusesAmbiguousOrForeignSettings(t *testing.T) {
	for _, test := range []struct{ name, payload string }{
		{"null document", `null`},
		{"nested null", `{"sequence":{"increment":null}}`},
		{"null encoding item", `{"encoding":[null]}`},
		{"incomplete literal", `{"literal":{}}`},
		{"null compression", `{"compression":null}`},
		{"duplicate family", `{"family":"hot","family":"cold"}`},
		{"common name", `{"name":"other"}`},
		{"common type", `{"type":{"typeId":"INT64"}}`},
		{"common nullability", `{"notNull":true}`},
		{"false drop", `{"dropDefault":false}`},
		{"overlapping defaults", `{"dropDefault":true,"literal":{}}`},
		{"unknown compression", `{"compression":{"algorithm":999}}`},
		{"sequence position", `{"sequence":{"setVal":{"nextValue":"7"}}}`},
		{"unknown setting", `{"surprise":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			definition := schema.ColumnDesiredModel()
			input := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: definition.Kind, Owner: columnOwner(), SchemaVersion: definition.SchemaVersion,
				Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(test.payload)}
			result, err := schema.NormalizeDesired(input)
			c.Assert(result, qt.IsNil)
			c.Assert(err, qt.IsNotNil)
		})
	}
}
