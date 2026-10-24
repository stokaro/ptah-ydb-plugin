package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
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
