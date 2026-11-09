package sourceencode_test

import (
	"bytes"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A secret a read reports names no variable, and is written with the
// variable Ptah names for its path, which reads back as written; the read's
// database is left to the host's connection. Ported from
// the host's TestRenderSecretsRoundTripThroughParser.
func TestSecretOfAReadIsWrittenWithTheDefaultVariable(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_YAML, protocol.SourceDocument_GO_ANNOTATIONS} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			owner := &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: "ext/aws", Name: "s3.key"}
			observed, err := schema.CaptureObject(owner, &native.SecretPresence{})
			c.Assert(err, qt.IsNil)
			desired, err := schema.ConvertNative(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			desired.Context = &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}
			value := &protocol.NormalizedValue{Id: "secret", Value: desired, NormalizedOwner: proto.CloneOf(desired.Owner),
				CanonicalFormat: schema.NativeDesiredModel(schema.SecretProperties).CanonicalFormat, CanonicalProjection: bytes.Clone(desired.Payload)}

			result, err := sourceencode.Secret(format, value)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Loss, qt.Equals, "")
			decoded, err := secretsource.Decode(c.Context(), &protocol.SourceDocument{Id: "export", Format: format,
				Fragments: []*protocol.SourceFragment{result.Parts[0].Fragment}})
			c.Assert(err, qt.IsNil)
			intent, err := schema.DesiredSecret(decoded[0][0].Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(intent.GetValueEnv(), qt.Equals, "PTAH_SECRET_EXT_AWS_S3_KEY")
		})
	}
}
