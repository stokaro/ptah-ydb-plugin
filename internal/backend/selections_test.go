package backend_test

import (
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/workload"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

func TestScopeSelectionsNegotiation(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	c.Assert(provider.ValidateDescription(description, description.Features), qt.IsNil)
	c.Assert(description.Features, qt.Contains, provider.StatementsFeature)
	without := provider.NegotiatedDescription(description, nil)
	c.Assert(without.Engines[0].ScopeSelections, qt.HasLen, 0)
	with := provider.NegotiatedDescription(description, []string{provider.ScopeSelectionsFeature})
	c.Assert(provider.ValidateDescription(with, []string{provider.ScopeSelectionsFeature}), qt.IsNil)
	var kinds []string
	for _, selection := range with.Engines[0].ScopeSelections {
		kinds = append(kinds, selection.Kind)
		switch selection.Kind {
		case schema.ResourcePoolClassifierProperties:
			c.Assert(selection.KeepsRequired, qt.DeepEquals, []string{schema.ResourcePoolProperties})
			c.Assert(selection.Naming, qt.Equals, protocol.ScopeSelection_DATABASE_WIDE)
		case schema.StreamingQueryProperties:
			c.Assert(selection.KeepsRequired, qt.DeepEquals, []string{schema.ResourcePoolProperties})
			c.Assert(selection.Naming, qt.Equals, protocol.ScopeSelection_IN_SCHEMA)
		case schema.ResourcePoolProperties:
			c.Assert(selection.Naming, qt.Equals, protocol.ScopeSelection_DATABASE_WIDE)
		default:
			c.Assert(selection.Naming, qt.Equals, protocol.ScopeSelection_IN_SCHEMA)
			c.Assert(selection.GrantObjectType, qt.Not(qt.Equals), "")
		}
	}
	c.Assert(kinds, qt.ContentEquals, []string{schema.SecretProperties, schema.StreamingQueryProperties,
		schema.CoordinationProperties, schema.TopicProperties, schema.ExternalSourceProperties, schema.ExternalTableProperties,
		schema.ReplicationProperties, schema.TransferProperties, schema.ResourcePoolProperties, schema.ResourcePoolClassifierProperties})
}

// Every object with an access list of its own has a selection, so the host
// reads a grant on it as a grant on that object, by the type the host's
// embedded engine names it with. The host spells the grant's target kind as
// the type in lower case, and the provider renders a GRANT on that kind.
func TestScopeSelectionsNameEveryObjectWithAnAccessList(t *testing.T) {
	tests := []struct {
		kind, selector, grant, capability string
		// directory holds the object; a resource pool belongs to the
		// database and has none.
		directory string
	}{
		{kind: schema.SecretProperties, selector: "secret", grant: "SECRET", capability: "secrets", directory: "app"},
		{kind: schema.StreamingQueryProperties, selector: "streaming_query", grant: "STREAMING QUERY", capability: "streaming_queries", directory: "app"},
		{kind: schema.CoordinationProperties, selector: "coordination_node", grant: "COORDINATION NODE", capability: "coordination_nodes", directory: "app"},
		{kind: schema.TopicProperties, selector: "topic", grant: "TOPIC", capability: "topics", directory: "app"},
		{kind: schema.ExternalSourceProperties, selector: "external_data_source", grant: "EXTERNAL DATA SOURCE", capability: "external_data_sources", directory: "app"},
		{kind: schema.ExternalTableProperties, selector: "external_table", grant: "EXTERNAL TABLE", capability: "external_data_sources", directory: "app"},
		{kind: schema.ReplicationProperties, selector: "async_replication", grant: "ASYNC REPLICATION", capability: "async_replication", directory: "app"},
		{kind: schema.TransferProperties, selector: "transfer", grant: "TRANSFER", capability: "transfers", directory: "app"},
		{kind: schema.ResourcePoolProperties, selector: "resource_pool", grant: "RESOURCE POOL", capability: "resource_pools"},
	}
	selections := provider.NegotiatedDescription(backend.Description(), []string{provider.ScopeSelectionsFeature}).Engines[0].ScopeSelections
	for _, test := range tests {
		t.Run(test.selector, func(t *testing.T) {
			c := testcheck.New(t)
			index := slices.IndexFunc(selections, func(selection *protocol.ScopeSelection) bool { return selection.Kind == test.kind })
			c.Assert(index >= 0, qt.IsTrue, qt.Commentf("no selection for %s", test.kind))
			c.Assert([]string{selections[index].SelectorType, selections[index].GrantObjectType}, qt.DeepEquals, []string{test.selector, test.grant})
			target := &protocol.Reference{Engine: "ydb", Kind: strings.ToLower(test.grant), Schema: test.directory, Name: "object"}
			rendered, err := backend.New().Render(c.Context(), &protocol.RenderRequest{Engine: "ydb", Form: protocol.RenderRequest_TEXT,
				TargetFeatures: []string{provider.CommonObjectsFeature, provider.CommonAccessFeature, provider.TextUnitsFeature},
				Capabilities:   map[string]bool{"transactional_ddl": false, "role_management": true, "relative_grant_paths": true, test.capability: true},
				Changes: []*protocol.Change{{Id: "grant", Object: target, Operation: &protocol.Change_CommonObject{CommonObject: &protocol.CommonObjectChange{
					Operation: &protocol.CommonObjectChange_Grant{Grant: &protocol.SQLGrant{Role: "reader", Privileges: []string{"SELECT"}, Target: target}}}}}}})
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Units, qt.HasLen, 1)
			c.Assert(rendered.Units[0].GetSql(), qt.Matches, "GRANT 'ydb.generic.read' ON `.*object` TO `reader`;")
		})
	}
}

func TestClassifierScopeKeepsItsPool(t *testing.T) {
	for _, representation := range []protocol.ValueInput_Representation{protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED} {
		t.Run(representation.String(), func(t *testing.T) {
			c := testcheck.New(t)
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
