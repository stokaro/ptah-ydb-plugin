package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
)

// TestDescriptionListsOneFeatureSet pins that the engine offers every
// service of the provider, dev realms, which the contract makes a feature of
// the engine alone, and the statement facts of its lint, which the contract
// lists beside the engine's lint.
func TestDescriptionListsOneFeatureSet(t *testing.T) {
	c := qt.New(t)

	description := backend.Description()

	c.Assert(description.Features, qt.Not(qt.HasLen), 0)
	c.Assert(description.Engines[0].Features, qt.DeepEquals, append(description.Features, "database.dev-realms", "lint.statement-facts"))
}

func TestDescriptionAdvertisesTheDevDatabaseService(t *testing.T) {
	c := qt.New(t)

	description := backend.Description()

	c.Assert(description.Features, qt.Contains, "database.dev")
	c.Assert(description.Engines[0].Features, qt.Contains, "database.dev")
}

// The lifecycle phase requires its declared contract minor, and later minors
// retain it. Older hosts must refuse before planning native replacements.
func TestDescriptionRequiresLifecycleContract(t *testing.T) {
	for _, test := range []struct {
		name string
		minor uint32
		want error
	}{
		{"earlier minor", 4, provider.ErrProtocol},
		{"lifecycle minor", 5, nil},
		{"later minor", 6, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			host := &protocol.ContractVersion{Major: provider.Revision, Minor: test.minor}
			c.Assert(provider.CheckContract(host, provider.MinimumContract(backend.Description())), qt.ErrorIs, test.want)
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
