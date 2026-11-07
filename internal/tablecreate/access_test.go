package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A table the plan creates leaves access unchanged: no role could read it
// before, and it takes its directory's permissions. The safety classifier
// reads an unknown access effect as destructive, which labeled every creation
// destructive where the built-in engine's CREATE TABLE is safe.
func TestCreatedTableLeavesAccessUnchanged(t *testing.T) {
	c := testcheck.New(t)
	request := creationPlan(c)
	request.CommonSteps = []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "create"}, Parent: request.Tables[0].Subject,
		Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a table",
			Effects: []*protocol.PlanningEffect{{Subject: request.Tables[0].Subject, Action: protocol.PlanningEffect_CREATE}}}}}

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	semantics := plan.Contributions[0].Steps[0].Operation.Value.OperationSemantics
	c.Assert(semantics.Access, qt.Equals, protocol.ChangeSemantics_UNCHANGED)
	c.Assert(semantics.AccessReason, qt.Equals, "The table is created by this plan, so no role could read it before; it takes its directory's permissions")
}

// A rebuild's scratch table is the one creation whose access stays unknown:
// it is created with its directory's permissions and takes those of the table
// whose rows it receives only with the copy. The copy, the renames and the
// drop keep their own assessments, and the alteration of a table that exists
// leaves access unchanged.
func TestRebuildScratchTableAccessIsUnknown(t *testing.T) {
	c := testcheck.New(t)
	request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	var access []protocol.ChangeSemantics_Access
	for _, step := range plan.Contributions[0].Steps {
		access = append(access, step.Operation.Value.OperationSemantics.Access)
	}
	c.Assert(access, qt.DeepEquals, []protocol.ChangeSemantics_Access{protocol.ChangeSemantics_ACCESS_UNKNOWN, protocol.ChangeSemantics_UNCHANGED,
		protocol.ChangeSemantics_UNCHANGED, protocol.ChangeSemantics_UNCHANGED, protocol.ChangeSemantics_NARROWS})
	c.Assert(plan.Contributions[0].Steps[0].Operation.Value.OperationSemantics.AccessReason, qt.Equals,
		"A rebuild's new table is created with its directory's permissions; the copy gives it those of the table whose rows it receives")
}

// Changing an existing table's column families is an alteration: YDB keeps
// the table's access control entries, so access is unchanged.
func TestAlteredTableLeavesAccessUnchanged(t *testing.T) {
	c := testcheck.New(t)
	request := alterRequest(c, alterCase{desiredTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`, heldTable: plainTable})

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	semantics := plan.Contributions[0].Steps[0].Operation.Value.OperationSemantics
	c.Assert(semantics.Access, qt.Equals, protocol.ChangeSemantics_UNCHANGED)
	c.Assert(semantics.AccessReason, qt.Equals, "Altering a table preserves its access metadata")
}
