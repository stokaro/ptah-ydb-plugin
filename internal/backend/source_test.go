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

// goNode captures one coordination node directive written on a struct that
// declares no table.
func goNode(attributes map[string]string) (*protocol.DecodeSourceRequest, error) {
	return testsource.GoDirectives(nil, []testsource.GoDeclaration{{Directive: schema.CoordinationGoDirective().Directive, Attributes: attributes, Struct: "Limits", File: "nodes.go", FileSize: 120,
		Start: testsource.GoPosition{Offset: 10, Line: 2, Column: 1}, End: testsource.GoPosition{Offset: 90, Line: 2, Column: 81}}})
}

// A Go directive declares the node the host's own directive of the same name
// declares, with the settings in the same model as a YAML entry.
func TestCoordinationGoSource_HappyPath(t *testing.T) {
	c := qt.New(t)
	request, err := goNode(map[string]string{"name": "limits", "schema": "app", "self_check_period": "PT2S", "session_grace_period": "PT15S",
		"read_consistency_mode": "strict", "attach_consistency_mode": "relaxed", "rate_limiter_counters_mode": "detailed"})
	c.Assert(err, qt.IsNil)
	result, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, result), qt.IsNil)
	values := result.Documents[0].Fragments[0].Values
	c.Assert(values, qt.HasLen, 1)
	c.Assert(values[0].Placement, qt.Equals, protocol.SourceModelValue_NATIVE_OBJECT)
	c.Assert(values[0].Value.Value.Owner.Schema, qt.Equals, "app")
	c.Assert(values[0].Value.Value.Owner.Name, qt.Equals, "limits")
	c.Assert(string(values[0].Value.Value.Payload), qt.Equals, `{"attach_consistency_mode":"relaxed","rate_limiter_counters_mode":"detailed","read_consistency_mode":"strict","self_check_period_millis":2000,"session_grace_period_millis":15000}`)
	c.Assert(values[0].Source.FragmentId, qt.Equals, "0")
	c.Assert(values[0].Source.Path, qt.HasLen, 0)
}

// A Go source that declares no node claims the namespace whole, as YAML and
// YQL do: an undeclared node is one the plan removes.
func TestCoordinationGoSourceClaimsTheNamespace_HappyPath(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.GoDirectives(nil, nil)
	c.Assert(err, qt.IsNil)
	result, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	var kinds []string
	for _, claim := range result.Documents[0].Coverage.Kinds {
		if claim.Knowledge.State == protocol.ModelKnowledge_COMPLETE {
			kinds = append(kinds, claim.Model.Kind)
		}
	}
	c.Assert(kinds, qt.Contains, schema.CoordinationProperties)
}

func TestCoordinationGoSource_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name       string
		attributes map[string]string
		want       string
	}{
		{"no name", map[string]string{"schema": "app"}, `.*ptah:schema:coordinationnode needs a name`},
		{"blank name", map[string]string{"name": "  "}, `.*ptah:schema:coordinationnode needs a name`},
		{"source provenance", map[string]string{"name": "limits", "struct_name": "Limits"}, `.*Go directive attribute is outside its claim`},
		{"clamped period", map[string]string{"name": "limits", "self_check_period": "PT20S"}, `.*coordination node "limits".*`},
		{"runtime lock node", map[string]string{"name": "ptah_locks"}, `.*reserved for provider runtime use`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request, err := goNode(test.attributes)
			c.Assert(err, qt.IsNil)
			result, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(result, qt.IsNil)
		})
	}
}

// Two directives that name one node are refused, as the host refuses a node
// declared twice.
func TestCoordinationGoSourceRefusesADuplicate_FailurePath(t *testing.T) {
	c := qt.New(t)
	declaration := testsource.GoDeclaration{Directive: schema.CoordinationGoDirective().Directive, Attributes: map[string]string{"name": "limits", "schema": "app"}, Struct: "Limits", File: "nodes.go", FileSize: 120,
		Start: testsource.GoPosition{Offset: 10, Line: 2, Column: 1}, End: testsource.GoPosition{Offset: 50, Line: 2, Column: 41}}
	again := declaration
	again.Struct, again.Start, again.End = "Again", testsource.GoPosition{Offset: 60, Line: 5, Column: 1}, testsource.GoPosition{Offset: 100, Line: 5, Column: 41}
	request, err := testsource.GoDirectives(nil, []testsource.GoDeclaration{declaration, again})
	c.Assert(err, qt.IsNil)
	result, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

// The provider claims the host's Go directives for the objects it owns, each
// once: the host's annotation tools list a directive only while some owner
// claims it.
func TestDescriptionClaimsTheGoDirectives_HappyPath(t *testing.T) {
	c := qt.New(t)
	var directives []string
	for _, claim := range backend.Description().Engines[0].GoSourceDirectives {
		directives = append(directives, claim.Directive)
	}
	c.Assert(directives, qt.DeepEquals, []string{"ptah:schema:table", "ptah:schema:index", "ptah:schema:columnfamily", "ptah:schema:changefeed",
		"ptah:schema:changefeed:consumer", "ptah:schema:coordinationnode", "ptah:schema:topic", "ptah:schema:topic:consumer"})
}
