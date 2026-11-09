package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/provider"

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

	lines := make(map[string]map[string]bool)
	for _, line := range listed.GetEngines()[0].GetReleaseLines() {
		lines[line.GetLine()] = line.GetCapabilities()
	}
	c.Assert(lines["25.1"]["vector_indexes"], qt.IsFalse)
	c.Assert(lines["25.2"]["vector_indexes"], qt.IsTrue)
	c.Assert(lines["26.2"], qt.DeepEquals, facts)

	unlisted := provider.NegotiatedDescription(backend.Description(), nil)
	c.Assert(unlisted.GetEngines()[0].GetDefaultCapabilities(), qt.IsNil)
	c.Assert(unlisted.GetEngines()[0].GetReleaseLines()[0].GetCapabilities(), qt.IsNil)
}

// The offline facts carry the facts only the host reads, on every line: it
// compares an index's covered columns under index_covering_columns, so an
// unchanged index with a COVER list plans no change, and it refuses
// --lock-timeout offline under migration_lock_timeout established false.
func TestDescriptionStatesTheFactsOnlyTheHostReads(t *testing.T) {
	c := testcheck.New(t)
	listed := provider.NegotiatedDescription(backend.Description(), []string{provider.DefaultCapabilitiesFeature, provider.CommonObjectsFeature})
	engine := listed.GetEngines()[0]
	c.Assert(engine.GetDefaultCapabilities()["index_covering_columns"], qt.IsTrue)
	lockTimeout, established := engine.GetDefaultCapabilities()["migration_lock_timeout"]
	c.Assert(established, qt.IsTrue)
	c.Assert(lockTimeout, qt.IsFalse)
	for _, line := range engine.GetReleaseLines() {
		c.Assert(line.GetCapabilities()["index_covering_columns"], qt.IsTrue, qt.Commentf("line %s", line.GetLine()))
		c.Assert(line.GetCapabilities()["rename_table"], qt.IsTrue, qt.Commentf("line %s", line.GetLine()))
	}
}
