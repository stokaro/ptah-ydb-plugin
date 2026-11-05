package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The engine states the facts an offline command plans it with to a host
// that lists engine.default-capabilities: nontransactional DDL established,
// which table creation requires, and the 26.2 facts a session states without
// a monitoring endpoint. A host that does not list the feature receives none.
func TestDescriptionStatesTheOfflineCapabilities(t *testing.T) {
	c := testcheck.New(t)
	listed := provider.NegotiatedDescription(backend.Description(), []string{provider.DefaultCapabilitiesFeature, provider.CommonObjectsFeature})
	c.Assert(provider.ValidateDescription(listed, nil), qt.IsNil)
	facts := listed.GetEngines()[0].GetDefaultCapabilities()
	transactional, established := facts["transactional_ddl"]
	c.Assert(established, qt.IsTrue)
	c.Assert(transactional, qt.IsFalse)
	c.Assert(facts["alter_column_default"], qt.IsTrue)
	c.Assert(facts["full_text_indexes"], qt.IsTrue)
	c.Assert(facts["resource_pools"], qt.IsFalse)

	unlisted := provider.NegotiatedDescription(backend.Description(), nil)
	c.Assert(unlisted.GetEngines()[0].GetDefaultCapabilities(), qt.IsNil)
}
