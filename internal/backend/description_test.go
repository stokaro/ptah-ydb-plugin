package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// TestDescriptionReturnsACopyTheCallerMayChange pins that a caller changing
// the description it was given changes neither the next caller's nor the one
// the provider checks its requests against, which every copy is made from.
func TestDescriptionReturnsACopyTheCallerMayChange(t *testing.T) {
	c := testcheck.New(t)
	before := backend.Description()

	changed := backend.Description()
	changed.Engines[0].ValueModels = nil
	changed.Features = nil

	after := backend.Description()
	c.Assert(proto.Equal(after, before), qt.IsTrue)
	c.Assert(after.Engines[0].ValueModels, qt.Not(qt.HasLen), 0)
	c.Assert(after.Features, qt.Not(qt.HasLen), 0)
}

// TestDescriptionStatesWhereTheConnectionReadsCapabilities pins the line the
// engine states under engine.connection-capability-source (contract 25.35):
// ptah db capabilities prints it beneath a capability the cluster's feature
// flags refine, where the host named the monitoring endpoint by the engine's
// name (stokaro/ptah#5770). The SDK checks the description with the line.
func TestDescriptionStatesWhereTheConnectionReadsCapabilities(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()

	c.Assert(description.GetEngines()[0].GetConnectionCapabilitySource(), qt.Equals,
		"read from this cluster's feature flags at its monitoring endpoint, not from its release line")
	c.Assert(description.GetEngines()[0].GetFeatures(), qt.Contains, provider.ConnectionCapabilitySourceFeature)
	c.Assert(provider.ValidateDescription(description, nil), qt.IsNil)
}
