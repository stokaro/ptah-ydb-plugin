package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestCoordinationSourcePreservesIntentAndTrace(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.Request(`{"coordination_nodes":{"z":{"name":"queue","schema":"app/workers","self_check_period":"PT0.5S","session_grace_period":"PT2S","read_consistency_mode":" STRICT ","rate_limiter_counters_mode":"DETAILED","struct_name":"Worker"},"default":{}}}`)
	c.Assert(err, qt.IsNil)
	before := proto.CloneOf(request)
	owner := backend.New()
	result, err := owner.DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, result), qt.IsNil)
	c.Assert(proto.Equal(before, request), qt.IsTrue)
	values := result.Documents[0].Fragments[0].Values
	c.Assert(values, qt.HasLen, 2)
	c.Assert(values[0].Value.Value.Owner.Name, qt.Equals, "default")
	c.Assert(string(values[0].Value.Value.Payload), qt.Equals, `{}`)
	c.Assert(values[1].Value.Value.Owner.Schema, qt.Equals, "app/workers")
	c.Assert(values[1].Value.Value.Owner.Name, qt.Equals, "queue")
	c.Assert(values[1].Value.Value.Owner.Database, qt.Equals, "")
	c.Assert(string(values[1].Value.Value.Payload), qt.Equals, `{"rate_limiter_counters_mode":"detailed","read_consistency_mode":"strict","self_check_period_millis":500,"session_grace_period_millis":2000,"struct_name":"Worker"}`)
	c.Assert(values[1].Source.Path[0].GetKey(), qt.Equals, "z")
	c.Assert(result.Documents[0].Coverage.Kinds, qt.HasLen, 1)
	c.Assert(result.Documents[0].Coverage.Kinds[0].Model.Representation, qt.Equals, protocol.ValueInput_DESIRED)
	c.Assert(result.Documents[0].Coverage.Subjects, qt.HasLen, 2)
	for _, value := range values {
		normalized, err := owner.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: value.Value.Id, Representation: protocol.ValueInput_DESIRED, Value: value.Value.Value}}})
		c.Assert(err, qt.IsNil)
		c.Assert(proto.Equal(normalized.Values[0], value.Value), qt.IsTrue)
	}
}

func TestCoordinationSourceRefusesWholeInvalidBatch(t *testing.T) {
	for _, bad := range []string{
		`null`, `[]`, `{"unknown":"ignored"}`, `{"read_consistency_mode":null}`, `{"read_consistency_mode":false}`,
		`{"self_check_period":1000}`, `{"self_check_period":"PT0S"}`, `{"self_check_period":"PT0.0001S"}`,
		`{"self_check_period":"PT0.499S"}`, `{"self_check_period":"PT10S"}`, `{"session_grace_period":"PT31S"}`,
		`{"read_consistency_mode":""}`, `{"attach_consistency_mode":"stale"}`, `{"rate_limiter_counters_mode":"none"}`,
		`{"name":""}`, `{"name":"a/b"}`, `{"schema":"../outside"}`, `{"schema":"/absolute"}`, `{"schema":"app//nested"}`,
		`{"name":".sys"}`, `{"name":"ptah_locks"}`, `{"name":"x","name":"y"}`,
	} {
		t.Run(bad, func(t *testing.T) {
			c := qt.New(t)
			request, err := testsource.Request(`{"coordination_nodes":{"good":{},"bad":` + bad + `}}`)
			c.Assert(err, qt.IsNil)
			result, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.IsNotNil)
			c.Assert(result, qt.IsNil)
		})
	}
	c := qt.New(t)
	duplicate, err := testsource.Request(`{"coordination_nodes":{"a":{"name":"same"},"b":{"name":"same"}}}`)
	c.Assert(err, qt.IsNil)
	result, err := backend.New().DecodeSource(c.Context(), duplicate)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

func TestCoordinationSourceKeepsEmptyScopeAndRejectsAttachments(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.Request(`{"coordination_nodes":{}}`)
	c.Assert(err, qt.IsNil)
	result, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Documents[0].Fragments[0].Values, qt.HasLen, 0)
	c.Assert(result.Documents[0].Coverage.Kinds, qt.HasLen, 1)
	request.Documents[0].Fragments = nil
	request.Documents[0].Models = nil
	result, err = backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Documents[0].Coverage.Kinds, qt.HasLen, 0)
	request, err = testsource.Request(`{"coordination_nodes":{"locks":{"schema":"app","name":"ptah_locks"}}}`)
	c.Assert(err, qt.IsNil)
	_, err = backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	request.Documents[0].Fragments[0].Subject = &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Name: "attached"}
	result, err = backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

func TestCoordinationSourceCanceledWithoutOutput(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.Request(`{"coordination_nodes":{"node":{}}}`)
	c.Assert(err, qt.IsNil)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	result, err := backend.New().DecodeSource(ctx, request)
	c.Assert(status.Code(err), qt.Equals, codes.Canceled)
	c.Assert(result, qt.IsNil)
}

func TestCoordinationSourceRefusesDeclaredTableCollision(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.Request(`{"coordination_nodes":{"node":{"schema":"app"}}}`)
	c.Assert(err, qt.IsNil)
	request.Documents[0].Bindings = []*protocol.SourceBinding{{Id: "table", EntryKey: "node", Object: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "node"}}}
	result, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.ErrorMatches, ".*conflicts with a declared table")
	c.Assert(result, qt.IsNil)
	request.Documents[0].Bindings[0].Object.Schema = "other"
	_, err = backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
}

func TestEmptyNormalizationHonorsCancellation(t *testing.T) {
	c := qt.New(t)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	result, err := backend.New().NormalizeValues(ctx, &protocol.NormalizeValuesRequest{Engine: "ydb"})
	c.Assert(status.Code(err), qt.Equals, codes.Canceled)
	c.Assert(result, qt.IsNil)
}
