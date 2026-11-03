package backend_test

import (
	"bytes"
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/topics"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

// groupingContext is the context of a call from a host that negotiates
// encoding groups, as Ptah's host does.
func groupingContext(c *qt.C) context.Context {
	return provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.HCLSourceFeature, provider.EncodeGroupsFeature})
}

// topicValue is the desired value of topic app/events with one consumer.
func topicValue(c *qt.C) *protocol.EncodeSourceValue {
	c.Helper()
	intent, err := topics.Intent(ydbdecl.Topic{MinActivePartitions: 2, Consumers: []ydbdecl.Consumer{{Name: "audit", Important: true}}})
	c.Assert(err, qt.IsNil)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.TopicProperties, Schema: "app", Name: "events"}
	value, err := schema.CaptureTopicIntent(owner, intent)
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT, Value: &protocol.NormalizedValue{Id: "topic", Value: value,
		NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: schema.NativeDesiredModel(schema.TopicProperties).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}}
}

// columnFacet is the desired facet of column payload of app/records.
func columnFacet(c *qt.C, payload string) *protocol.EncodeSourceValue {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: "column", Schema: "app", Path: []string{"records"}, Name: "payload"}
	model := schema.ColumnDesiredModel()
	envelope, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: "records", Value: &protocol.NormalizedValue{Id: "column",
		Value: envelope, NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(envelope.Payload)}}
}

// nativeFacet is the desired facet of kind of app/records with payload.
func nativeFacet(c *qt.C, id, kind, payload string) *protocol.EncodeSourceValue {
	c.Helper()
	table := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}
	model := schema.NativeDesiredModel(kind)
	envelope, err := schema.NormalizeDesired(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: model.SchemaVersion,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: table, Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_FACET, BindingId: "records", Value: &protocol.NormalizedValue{Id: id,
		Value: envelope, NormalizedOwner: proto.CloneOf(table), CanonicalFormat: model.CanonicalFormat, CanonicalProjection: bytes.Clone(envelope.Payload)}}
}

// With encoding groups, a topic and its consumer, and a table's settings
// with the family that holds a column, are each written by one group of
// several parts, and decoding the parts gives every value back. The empty
// changefeed and native index sets a read reports for every table are
// written by the table's own part and decode to nothing.
func TestEncodeSource_Groups_HappyPath(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			ctx := groupingContext(c)
			table := tableFacet(c, `{"columnFamilies":[{"name":"cold","compression":"COMPRESSION_LZ4"}],"keyBloomFilter":"ENABLED"}`)
			column := columnFacet(c, `{"family":"cold"}`)
			request := tableRequest(format, topicValue(c), table, column, nativeFacet(c, "feeds", schema.ChangefeedProperties, `{}`),
				nativeFacet(c, "indexes", schema.IndexProperties, `{}`))
			encoded, err := backend.New().EncodeSource(ctx, request)
			c.Assert(err, qt.IsNil)
			c.Assert(encoded.Losses, qt.HasLen, 0)
			c.Assert(encoded.Fragments, qt.HasLen, 0)
			c.Assert(encoded.Groups, qt.HasLen, 2)
			c.Assert(encoded.Groups[0].ValueIds, qt.DeepEquals, []string{"topic"})
			c.Assert(encoded.Groups[1].ValueIds, qt.DeepEquals, []string{"records", "feeds", "column", "indexes"})
			c.Assert(decodedProjections(c, ctx, sourceDocument(format, request, encoded)), qt.DeepEquals, map[string]string{
				schema.TopicProperties + " app/events":   string(request.Values[0].Value.CanonicalProjection),
				schema.TableProperties + " app/records":  string(table.Value.CanonicalProjection),
				schema.ColumnProperties + " app/payload": string(column.Value.CanonicalProjection),
			})
		})
	}
}

// Without encoding groups, a value that needs several declarations is a loss
// that names it, and a value one declaration writes is a flat fragment.
func TestEncodeSource_WithoutGroups_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS, topicValue(c), tableFacet(c, `{"keyBloomFilter":"ENABLED"}`))
	encoded, err := backend.New().EncodeSource(encodingContext(c), request)
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Groups, qt.HasLen, 0)
	c.Assert(encoded.Fragments, qt.HasLen, 1)
	c.Assert(encoded.Fragments[0].ValueIds, qt.DeepEquals, []string{"records"})
	c.Assert(encoded.Losses, qt.CmpEquals(protocmp.Transform()), []*protocol.EncodeLoss{{ValueId: "topic",
		Message: "topic app/events needs several declarations, which a host that does not negotiate source.encode-groups cannot write"}})
}

// A Go export gives each column whose YDB type has a Go type that type, with
// the import it needs, and leaves a type it does not know to the host.
func TestEncodeSource_FieldTypes_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS)
	request.Columns = []*protocol.EncodeSourceColumn{
		{BindingId: "records", Name: "id", Type: "Uint64", PrimaryKey: true},
		{BindingId: "records", Name: "at", Type: "Timestamp", Nullable: true},
		{BindingId: "records", Name: "amount", Type: "Decimal(22,9)", Nullable: true},
		{BindingId: "records", Name: "tags", Type: "List<Utf8>", Nullable: true},
	}
	encoded, err := backend.New().EncodeSource(groupingContext(c), request)
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.FieldTypes, qt.CmpEquals(protocmp.Transform()), []*protocol.EncodedFieldType{
		{BindingId: "records", Column: "id", GoType: "uint64"},
		{BindingId: "records", Column: "at", GoType: "time.Time", GoImport: "time"},
		{BindingId: "records", Column: "amount", GoType: "types.Decimal", GoImport: "github.com/ydb-platform/ydb-go-sdk/v3/table/types"},
	})
}

// A nullable key column loses its table in a Go export, which the host then
// refuses with the line: YDB takes one, and no declaration can ask for it, so
// the models would rebuild the column NOT NULL.
func TestEncodeSource_NullableKey_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS, tableFacet(c, `{"keyBloomFilter":"ENABLED"}`))
	request.Columns = []*protocol.EncodeSourceColumn{{BindingId: "records", Name: "id", Type: "Uint64", Nullable: true, PrimaryKey: true}}
	encoded, err := backend.New().EncodeSource(groupingContext(c), request)
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.Groups, qt.HasLen, 0)
	c.Assert(encoded.Losses, qt.CmpEquals(protocmp.Transform()), []*protocol.EncodeLoss{{ValueId: "records",
		Message: `column "id" of table "app/records" is a nullable key column, which no Ptah declaration can represent: Ptah writes NOT NULL on every YDB key column`}})
}

// A nullable key column of a table the request holds no facet of refuses the
// export, since no value can carry the loss.
func TestEncodeSource_NullableKeyWithoutFacets_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := tableRequest(protocol.SourceDocument_GO_ANNOTATIONS)
	request.Columns = []*protocol.EncodeSourceColumn{{BindingId: "records", Name: "id", Type: "Uint64", Nullable: true, PrimaryKey: true}}
	encoded, err := backend.New().EncodeSource(groupingContext(c), request)
	c.Assert(err, qt.ErrorMatches, `.*column "id" of table "app/records" is a nullable key column, which no Ptah declaration can represent: `+
		`Ptah writes NOT NULL on every YDB key column`)
	c.Assert(encoded, qt.IsNil)
}
