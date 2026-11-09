package inspect

// This white-box test checks reserve, which only the directory walk calls: a
// public read reaches it only through a server holding a reserved object, and
// the live host tests cover that path.
import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A read that carries views on the common wire leaves the native view model out
// of a reservation. An unrepresentable view subject would make the host require
// an object comparison for a model that has none.
func TestReservationLeavesTheNativeViewModelToTheCommonWire(t *testing.T) {
	for _, test := range []struct {
		name        string
		commonViews bool
		claimed     bool
	}{
		{"views on the common wire", true, false},
		{"views read as native objects", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			snapshot := &protocol.Schema{Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED}}
			Common{Views: test.commonViews}.reserve(snapshot, &protocol.Reference{Engine: "ydb", Database: "/local", Kind: schema.CoordinationProperties, Name: "ptah_locks"}, protocol.ReservedObject_LOCK)
			c.Assert(snapshot.ReservedObjects, qt.HasLen, 1)
			c.Assert(snapshot.ReservedObjects[0].ConflictKinds, qt.Contains, schema.CoordinationProperties)
			c.Assert(slices.Contains(snapshot.ReservedObjects[0].ConflictKinds, schema.ViewProperties), qt.Equals, test.claimed)
			c.Assert(slices.ContainsFunc(snapshot.Coverage.Subjects, func(subject *protocol.ModelSubjectCoverage) bool {
				return subject.Kind == schema.ViewProperties
			}), qt.Equals, test.claimed)
		})
	}
}
