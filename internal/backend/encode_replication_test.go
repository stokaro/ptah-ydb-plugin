package backend_test

import (
	"bytes"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Ported from the host's TestRender_Replication_RoundTrip: the source export
// keeps the connection, ordered items, consistency, and transfer lambdas.
func TestEncodeSource_ReplicationAndTransfersRoundTrip_HappyPath(t *testing.T) {
	for _, test := range []struct {
		format     protocol.SourceDocument_Format
		directives []string
	}{
		{format: protocol.SourceDocument_YAML, directives: []string{"", "", ""}},
		{format: protocol.SourceDocument_GO_ANNOTATIONS, directives: []string{
			"ptah:schema:async_replication", "ptah:schema:async_replication:item", "ptah:schema:async_replication:item",
			"ptah:schema:transfer", "ptah:schema:transfer",
		}},
	} {
		t.Run(test.format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			values := replicationValues(c)
			request := encodeRequest(test.format, values...)
			ctx := provider.ContextWithHostFeatures(c.Context(), []string{provider.EntryKeysFeature, provider.EncodeGroupsFeature,
				provider.PlanningObjectContextFeature})

			encoded, err := backend.New().EncodeSource(ctx, request)

			c.Assert(err, qt.IsNil)
			c.Assert(encoded.GetLosses(), qt.HasLen, 0)
			parts := provider.EncodedSourceParts(encoded)
			c.Assert(parts, qt.HasLen, len(test.directives))
			var directives []string
			for _, part := range parts {
				directives = append(directives, part.GetFragment().GetGoDirective().GetDirective())
			}
			c.Assert(directives, qt.DeepEquals, test.directives)
			want := make(map[string]string, len(values))
			for _, value := range values {
				owner := value.Value.Value.Owner
				want[owner.Kind+" "+owner.Schema+"/"+owner.Name] = string(value.Value.CanonicalProjection)
			}
			c.Assert(decodedProjections(c, ctx, sourceDocument(test.format, request, encoded)), qt.DeepEquals, want)
		})
	}
}

func TestEncodeSource_ReplicationAndTransferHaveHCLLosses(t *testing.T) {
	c := testcheck.New(t)
	request := encodeRequest(protocol.SourceDocument_HCL, replicationValues(c)...)
	encoded, err := backend.New().EncodeSource(groupedEncodingContext(c), request)
	c.Assert(err, qt.IsNil)
	c.Assert(encoded.GetLosses(), qt.HasLen, 3)
	c.Assert(provider.EncodedSourceParts(encoded), qt.HasLen, 0)
}

func replicationValues(c *qt.C) []*protocol.EncodeSourceValue {
	c.Helper()
	async := replication.ReplicationSpec{
		Connection: replication.Connection{ConnectionString: "grpcs://primary:2135/?database=/prod", User: "replicator",
			PasswordSecretPath: "secrets/password"},
		Items:            []replication.Item{{Source: "ledger", Target: "replica/ledger"}, {Source: "archive", Target: "replica/archive"}},
		ConsistencyLevel: replication.ConsistencyGlobal, CommitInterval: "PT1.5S",
	}
	transfer := replication.TransferSpec{
		Connection: replication.Connection{ConnectionString: "grpc://primary:2136/?database=/prod", TokenSecretName: "remote-token"},
		Source:     "events", Target: "rows", Lambda: "($msg) -> {\n  return [<| text: \"it's quoted\" |>];\n}",
		Consumer: "writer", BatchSizeBytes: 2048, FlushInterval: "PT3S",
	}
	plain := replication.TransferSpec{Source: "local-events", Target: "local-rows", Lambda: "($msg) -> { return [<| id: $msg._offset |>]; }"}
	var values []*protocol.EncodeSourceValue
	for i, spec := range []any{async, transfer, plain} {
		kind := schema.TransferProperties
		if i == 0 {
			kind = schema.ReplicationProperties
		}
		owner := &protocol.Reference{Engine: "ydb", Kind: kind, Schema: "app", Name: []string{"replicate", "copy", "local"}[i]}
		var value *protocol.Envelope
		var err error
		switch typed := spec.(type) {
		case replication.ReplicationSpec:
			value, err = schema.CaptureReplicationIntent(owner, typed)
		case replication.TransferSpec:
			value, err = schema.CaptureTransferIntent(owner, typed)
		}
		c.Assert(err, qt.IsNil)
		values = append(values, &protocol.EncodeSourceValue{Placement: protocol.SourceModelValue_NATIVE_OBJECT,
			Value: &protocol.NormalizedValue{Id: []string{"replication", "transfer", "plain"}[i], Value: value,
				NormalizedOwner: proto.CloneOf(owner), CanonicalFormat: schema.NativeDesiredModel(kind).CanonicalFormat,
				CanonicalProjection: bytes.Clone(value.Payload)}})
	}
	return values
}
