package sourceencode_test

import (
	"bytes"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/externals"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

func normalized(c *qt.C, kind string, intent proto.Message) *protocol.NormalizedValue {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: kind, Schema: "app/eu", Name: "a.b"}
	var value *protocol.Envelope
	var err error
	if secret, ok := intent.(*native.SecretIntent); ok {
		value, err = schema.CaptureSecretIntent(owner, secret)
	} else {
		value, err = schema.CaptureExternalIntent(owner, intent)
	}
	c.Assert(err, qt.IsNil)
	return &protocol.NormalizedValue{Id: "value", Value: value, NormalizedOwner: proto.CloneOf(owner),
		CanonicalFormat: schema.NativeDesiredModel(kind).CanonicalFormat, CanonicalProjection: bytes.Clone(value.Payload)}
}

func sourceIntent(c *qt.C) *native.ExternalSourceIntent {
	c.Helper()
	value, err := externals.SourceIntent(ydbdecl.ExternalDataSource{SourceType: "PostgreSQL", Location: "pg:5432", AuthMethod: "BASIC",
		Options: map[string]string{"LOGIN": "reader", "PASSWORD_SECRET_PATH": "/local/app/pw", "DATABASE_NAME": `a;b\c=d`}})
	c.Assert(err, qt.IsNil)
	return value
}

func tableIntent(c *qt.C) *native.ExternalTableIntent {
	c.Helper()
	value, err := externals.TableIntent(ydbdecl.ExternalTable{DataSource: "app/s3", Location: "rows/",
		Columns: []ydbdecl.ExternalColumn{{Name: "key", Type: "Int64", NotNull: true}, {Name: "total", Type: "Decimal(22,9)"}, {Name: "with space", Type: "Utf8"}},
		Options: map[string]string{"FORMAT": "csv_with_names", "CSV_DELIMITER": ";"}})
	c.Assert(err, qt.IsNil)
	return value
}

func TestNativeFragmentsRoundTripWithoutMutatingInputs(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS} {
		for _, kind := range []string{schema.SecretProperties, schema.ExternalSourceProperties, schema.ExternalTableProperties} {
			t.Run(format.String()+"/"+kind, func(t *testing.T) {
				c := qt.New(t)
				var intent proto.Message
				encode := sourceencode.Secret
				decode := secretsource.Decode
				switch kind {
				case schema.SecretProperties:
					intent = &native.SecretIntent{ValueEnv: "PTAH_SECRET_REFERENCE"}
				case schema.ExternalSourceProperties:
					intent, encode, decode = sourceIntent(c), sourceencode.ExternalSource, externalsource.Decode
				case schema.ExternalTableProperties:
					intent, encode, decode = tableIntent(c), sourceencode.ExternalTable, externalsource.Decode
				}
				input := normalized(c, kind, intent)
				before := proto.CloneOf(input)
				result, err := encode(format, input)
				c.Assert(err, qt.IsNil)
				c.Assert(result.Loss, qt.Equals, "")
				c.Assert(result.Parts, qt.HasLen, 1)
				c.Assert(proto.Equal(input, before), qt.IsTrue)
				decoded, err := decode(c.Context(), &protocol.SourceDocument{Id: "export", Format: format,
					Fragments: []*protocol.SourceFragment{result.Parts[0].Fragment}})
				c.Assert(err, qt.IsNil)
				c.Assert(decoded[0], qt.HasLen, 1)
				c.Assert(proto.Equal(decoded[0][0].Value.Value.Owner, input.Value.Owner), qt.IsTrue)
				c.Assert(decoded[0][0].Value.CanonicalProjection, qt.DeepEquals, input.CanonicalProjection)
				again, err := encode(format, input)
				c.Assert(err, qt.IsNil)
				c.Assert(proto.Equal(again.Parts[0], result.Parts[0]), qt.IsTrue)
			})
		}
	}
}

func TestNativeFragmentsRefuseLostFactsWithoutPartialOutput(t *testing.T) {
	c := qt.New(t)
	for _, test := range []struct {
		name   string
		value  *protocol.NormalizedValue
		encode func(protocol.SourceDocument_Format, *protocol.NormalizedValue) (sourceencode.Result, error)
	}{
		{name: "secret HCL", value: normalized(c, schema.SecretProperties, &native.SecretIntent{ValueEnv: "PTAH_SECRET_REFERENCE"}), encode: sourceencode.Secret},
		{name: "external source HCL", value: normalized(c, schema.ExternalSourceProperties, sourceIntent(c)), encode: sourceencode.ExternalSource},
		{name: "external table HCL", value: normalized(c, schema.ExternalTableProperties, tableIntent(c)), encode: sourceencode.ExternalTable},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := test.encode(protocol.SourceDocument_HCL, test.value)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Loss, qt.Not(qt.Equals), "")
			c.Assert(result.Parts, qt.HasLen, 0)
		})
	}
	source := sourceIntent(c)
	source.Access = &native.AccessIntent{Owner: new("reader")}
	value := normalized(c, schema.ExternalSourceProperties, source)
	result, err := sourceencode.ExternalSource(protocol.SourceDocument_YAML, value)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Loss, qt.Contains, "cannot preserve all")
	c.Assert(result.Parts, qt.HasLen, 0)
	bound := normalized(c, schema.SecretProperties, &native.SecretIntent{ValueEnv: "PTAH_SECRET_REFERENCE"})
	bound.Value.Owner.Database, bound.NormalizedOwner.Database = "/tenant", "/tenant"
	result, err = sourceencode.Secret(protocol.SourceDocument_YAML, bound)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Loss, qt.Contains, "source binding")
	c.Assert(result.Parts, qt.HasLen, 0)
}

func TestNativeFragmentsRejectMalformedValues(t *testing.T) {
	c := qt.New(t)
	value := normalized(c, schema.SecretProperties, &native.SecretIntent{ValueEnv: "PTAH_SECRET_REFERENCE"})
	value.Value.Payload = []byte(`{"value_env":"SECRET_INLINE"}`)
	result, err := sourceencode.Secret(protocol.SourceDocument_YAML, value)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result.Parts, qt.HasLen, 0)
	c.Assert(result.Loss, qt.Equals, "")
}

func TestExternalSourceReportsIncompleteIntentAsLoss(t *testing.T) {
	c := qt.New(t)
	value := normalized(c, schema.ExternalSourceProperties, &native.ExternalSourceIntent{SourceType: new("ObjectStorage"), Location: new("bucket")})
	result, err := sourceencode.ExternalSource(protocol.SourceDocument_YAML, value)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Loss, qt.Contains, "complete external data source")
	c.Assert(result.Parts, qt.HasLen, 0)
}

func TestExternalTableReportsIncompleteIntentAsLoss(t *testing.T) {
	c := qt.New(t)
	intent := tableIntent(c)
	intent.Location = new("")
	value := normalized(c, schema.ExternalTableProperties, intent)
	result, err := sourceencode.ExternalTable(protocol.SourceDocument_YAML, value)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Loss, qt.Contains, "complete external table")
	c.Assert(result.Parts, qt.HasLen, 0)
}

func TestExternalColumnNameThatGoCannotRepresentRemainsExactInYAML(t *testing.T) {
	for _, name := range []string{"part,whole", "quoted`name"} {
		t.Run(name, func(t *testing.T) {
			c := qt.New(t)
			intent := tableIntent(c)
			intent.Columns[0].Name = name
			value := normalized(c, schema.ExternalTableProperties, intent)
			before := proto.CloneOf(value)
			goResult, err := sourceencode.ExternalTable(protocol.SourceDocument_GO_ANNOTATIONS, value)
			c.Assert(err, qt.IsNil)
			c.Assert(goResult.Loss, qt.Not(qt.Equals), "")
			c.Assert(goResult.Parts, qt.HasLen, 0)
			yamlResult, err := sourceencode.ExternalTable(protocol.SourceDocument_YAML, value)
			c.Assert(err, qt.IsNil)
			c.Assert(yamlResult.Loss, qt.Equals, "")
			c.Assert(yamlResult.Parts, qt.HasLen, 1)
			decoded, err := externalsource.Decode(c.Context(), &protocol.SourceDocument{Id: "export", Format: protocol.SourceDocument_YAML,
				Fragments: []*protocol.SourceFragment{yamlResult.Parts[0].Fragment}})
			c.Assert(err, qt.IsNil)
			c.Assert(decoded[0], qt.HasLen, 1)
			c.Assert(decoded[0][0].Value.CanonicalProjection, qt.DeepEquals, value.CanonicalProjection)
			c.Assert(proto.Equal(value, before), qt.IsTrue)
		})
	}
}
