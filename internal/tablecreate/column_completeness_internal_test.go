// White-box testing required: completeColumnAlteration is the last check that
// a parent receipt does not acknowledge a column change no operation carries.
// Through Plan, rebuildNeeds and planSequences answer every Serial change
// before it, so the check is reachable only when one of them misses the
// change, which a black-box request cannot arrange.

package tablecreate

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// serialPlanningTable is app.records with key id declared as given and held
// as an Int64 that is a Serial when autoIncrement holds, whose sequence steps
// by 5 from 100.
func serialPlanningTable(id *protocol.DeclaredColumn, autoIncrement bool) *protocol.PlanningTable {
	id.Name, id.Primary = "id", true
	return &protocol.PlanningTable{Action: protocol.PlanningTable_ALTER,
		Desired: &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"}, Columns: []*protocol.DeclaredColumn{id}},
		Current: &protocol.ObservedTable{Name: "records", Schema: "app", Columns: []*protocol.ObservedColumn{{Name: "id", DataType: "Int64", IsNullable: "NO",
			PrimaryKey: true, AutoIncrement: autoIncrement, IdentityStart: "100", IdentityIncrement: "5"}}}}
}

// A Serial change no operation carries is refused rather than acknowledged.
func TestCompleteColumnAlterationSerial_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name          string
		id            *protocol.DeclaredColumn
		autoIncrement bool
		want          string
	}{
		{name: "another increment", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "7"}, autoIncrement: true,
			want: `Column "id" sequence change requires an owned column operation`},
		{name: "a column that becomes a Serial", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "5"},
			want: `Column "id" definition change requires an owned column operation`},
		{name: "a Serial that stops being one", id: &protocol.DeclaredColumn{Type: "Int64"}, autoIncrement: true,
			want: `Column "id" definition change requires an owned column operation`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "records"}
			caps := map[string]bool{"serial_columns": true, "serial_sequence_options": true}
			c.Assert(completeColumnAlteration(owner, serialPlanningTable(test.id, test.autoIncrement), nil, nil, nil, nil, nil, caps), qt.Equals, test.want)
		})
	}
}

// A sequence change its step carries, and a sequence that already holds the
// declaration, complete the alteration.
func TestCompleteColumnAlterationSerial_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		id    *protocol.DeclaredColumn
		steps []sequenceStep
	}{
		{name: "a change with its step", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "7"}, steps: []sequenceStep{{name: "id"}}},
		{name: "a sequence that holds the declaration", id: &protocol.DeclaredColumn{Type: "BIGINT", AutoIncrement: true, IdentityStart: "100", IdentityIncrement: "5"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: "app", Name: "records"}
			caps := map[string]bool{"serial_columns": true, "serial_sequence_options": true}
			c.Assert(completeColumnAlteration(owner, serialPlanningTable(test.id, true), nil, nil, nil, nil, test.steps, caps), qt.Equals, "")
		})
	}
}
