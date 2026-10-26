package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

func TestScopeSelectionsNegotiation(t *testing.T) {
	c := qt.New(t)
	description := backend.Description()
	c.Assert(provider.ValidateDescription(description, description.Features), qt.IsNil)
	c.Assert(description.Features, qt.Contains, provider.StatementsFeature)
	without := provider.NegotiatedDescription(description, nil)
	c.Assert(without.Engines[0].ScopeSelections, qt.HasLen, 0)
	with := provider.NegotiatedDescription(description, []string{provider.ScopeSelectionsFeature})
	c.Assert(provider.ValidateDescription(with, []string{provider.ScopeSelectionsFeature}), qt.IsNil)
	c.Assert(with.Engines[0].ScopeSelections, qt.HasLen, 6)
	for _, selection := range with.Engines[0].ScopeSelections {
		switch selection.Kind {
		case schema.ResourcePoolClassifierProperties:
			c.Assert(selection.KeepsRequired, qt.DeepEquals, []string{schema.ResourcePoolProperties})
			c.Assert(selection.Naming, qt.Equals, protocol.ScopeSelection_DATABASE_WIDE)
		case schema.ResourcePoolProperties:
			c.Assert(selection.Naming, qt.Equals, protocol.ScopeSelection_DATABASE_WIDE)
		default:
			c.Assert(selection.Naming, qt.Equals, protocol.ScopeSelection_IN_SCHEMA)
			c.Assert(selection.GrantObjectType, qt.Not(qt.Equals), "")
		}
	}
}

func TestClassifierScopeKeepsItsPool(t *testing.T) {
	for _, representation := range []protocol.ValueInput_Representation{protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED} {
		t.Run(representation.String(), func(t *testing.T) {
			c := qt.New(t)
			owner := &protocol.Reference{Engine: "ydb", Kind: schema.ResourcePoolClassifierProperties, Name: "batch.reader"}
			settings := schema.ClassifierSettings(workload.ClassifierSpec{ResourcePool: "batch.pool", Rank: 1})
			capture := schema.CaptureWorkloadIntent
			if representation == protocol.ValueInput_OBSERVED {
				owner.Database = "/local"
				capture = schema.CaptureWorkloadObservation
			}
			value, err := capture(owner, settings)
			c.Assert(err, qt.IsNil)
			request := &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "classifier", Representation: representation, Value: value}}}
			normalized, err := backend.New().NormalizeValues(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(normalized.Values[0].RequiredObjects, qt.HasLen, 1)
			pool := normalized.Values[0].RequiredObjects[0]
			c.Assert(pool.Kind, qt.Equals, schema.ResourcePoolProperties)
			c.Assert(pool.GetName().GetSource(), qt.Equals, "batch.pool")
			c.Assert(pool.GetSchema().GetSource(), qt.Equals, "")
			c.Assert(pool.GetDatabase().GetSource(), qt.Equals, owner.Database)
		})
	}
}
