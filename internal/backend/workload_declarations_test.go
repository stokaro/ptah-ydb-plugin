package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// declaredClassifier is the classifier name declared to route to pool at
// rank.
func declaredClassifier(c *qt.C, name, pool string, rank int64) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolClassifierProperties, Name: name},
		schema.ClassifierSettings(workload.ClassifierSpec{ResourcePool: pool, Rank: rank}))
	c.Assert(err, qt.IsNil)
	return value
}

// declaredPool is the pool name declared with no setting.
func declaredPool(c *qt.C, name string) *protocol.Envelope {
	c.Helper()
	value, err := schema.CaptureWorkloadIntent(&protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolProperties, Name: name}, schema.PoolSettings(workload.PoolSpec{}))
	c.Assert(err, qt.IsNil)
	return value
}

// A declared classifier that names a pool the declarations leave out, and
// that is not the pool default, is refused before anything is planned, in the
// words of the host's embedded engine: YDB would run the member's queries in
// the pool default without a word. Pool names are exact, so a pool declared
// in another case is another pool. Ported from the host's
// TestWorkloadDeclarationRefusesAnUndeclaredRoutingDestination and
// TestCheckRouting_FailurePath.
func TestAuthoredClassifierRoutingToAnUndeclaredPool_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	query, err := schema.CaptureStreamingQueryIntent(&protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Name: "query"}, streaming.Spec{Text: "SELECT 1;"}, false)
	c.Assert(err, qt.IsNil)
	for _, test := range []struct {
		name    string
		values  []*protocol.Envelope
		object  uint32
		message string
	}{
		{name: "no pool declared", values: []*protocol.Envelope{query, declaredClassifier(c, "route", "missing", 0)}, object: 1,
			message: `resource pool classifier "route": it names resource pool "missing", which is not declared; ` +
				`YDB runs the queries of a classifier whose pool does not exist in the pool "default" without a word`},
		{name: "a pool declared in another case", values: []*protocol.Envelope{declaredPool(c, "batch"), declaredClassifier(c, "a", "Batch", 1)}, object: 1,
			message: `resource pool classifier "a": it names resource pool "Batch", which is not declared; ` +
				`YDB runs the queries of a classifier whose pool does not exist in the pool "default" without a word`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := declarationRequest(c, test.values...)
			request.Capabilities[streaming.Capability] = true

			result, err := backend.New().PlanDeclarations(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 1)
			c.Assert(result.Diagnostics[0].GetObject(), qt.Equals, test.object)
			c.Assert(result.Diagnostics[0].Problem.Kind, qt.Equals, schema.ResourcePoolClassifierProperties)
			c.Assert(result.Diagnostics[0].Problem.Message, qt.Equals, test.message)
			c.Assert(result.Declarations, qt.HasLen, 0)
			c.Assert(result.Contributions, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
		})
	}
}

// A declared classifier may route to a pool the declarations hold, or to the
// pool default, which YDB creates with the database, without declaring it.
func TestAuthoredClassifierRoutingToADeclaredPoolOrTheDefault_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	for _, test := range []struct {
		name   string
		values []*protocol.Envelope
	}{
		{name: "a declared pool", values: []*protocol.Envelope{declaredPool(c, "batch"), declaredClassifier(c, "a", "batch", 1)}},
		{name: "the pool default", values: []*protocol.Envelope{declaredClassifier(c, "a", workload.DefaultPool, 1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := declarationRequest(c, test.values...)

			result, err := backend.New().PlanDeclarations(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Diagnostics, qt.HasLen, 0)
			c.Assert(result.Declarations, qt.HasLen, len(test.values))
			c.Assert(provider.ValidatePlannedDeclarations(backend.Description(), request, result), qt.IsNil)
		})
	}
}
