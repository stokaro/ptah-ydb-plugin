package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"

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
