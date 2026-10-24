package backend_test

import (
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
)

func TestDescriptionListsOneFeatureSet(t *testing.T) {
	c := qt.New(t)

	description := backend.Description()

	c.Assert(description.Engines[0].Features, qt.Not(qt.HasLen), 0)
	c.Assert(description.Engines[0].Features, qt.DeepEquals, description.Features)
}

func TestDescriptionAdvertisesTheDevDatabaseService(t *testing.T) {
	c := qt.New(t)

	description := backend.Description()

	c.Assert(description.Features, qt.Contains, "database.dev")
	c.Assert(description.Engines[0].Features, qt.Contains, "database.dev")
}

// TestDescriptionStartsOnEveryHostOfItsMajor pins the declared minimum: every
// addition after contract 25.0 the provider uses is negotiated, so a host of
// contract 25.0 starts it, and so does every later minor.
func TestDescriptionStartsOnEveryHostOfItsMajor(t *testing.T) {
	for minor := range provider.ContractMinor + 1 {
		t.Run(fmt.Sprintf("%d.%d", provider.Revision, minor), func(t *testing.T) {
			c := qt.New(t)
			host := &protocol.ContractVersion{Major: provider.Revision, Minor: minor}

			c.Assert(provider.CheckContract(host, provider.MinimumContract(backend.Description())), qt.IsNil)
		})
	}
}

// TestDescriptionRefusesAnotherMajor_FailurePath is the control: the declared
// minimum is a real version, not one every host meets.
func TestDescriptionRefusesAnotherMajor_FailurePath(t *testing.T) {
	c := qt.New(t)
	host := &protocol.ContractVersion{Major: provider.Revision - 1, Minor: provider.ContractMinor}

	err := provider.CheckContract(host, provider.MinimumContract(backend.Description()))

	c.Assert(err, qt.ErrorIs, provider.ErrProtocol)
}
