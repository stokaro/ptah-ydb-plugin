package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
)

// heldGrant is a grant the read found on app.records.
func heldGrant(role, privilege string, implicit bool) *protocol.ObservedGrant {
	return &protocol.ObservedGrant{Role: role, Privilege: privilege, Implicit: implicit,
		Target: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}}
}

// grantedRebuild is rebuildRequest with a rebuild in the policy, from a host
// that sends the held table's grants (planning.table-grants) as grants.
func grantedRebuild(c *qt.C, grants *protocol.PlanningTableGrants) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	request.TargetFeatures = append(request.TargetFeatures, provider.TableGrantsFeature, provider.CommonObjectsFeature)
	request.Tables[0].CurrentGrants = grants
	return request
}

// rebuiltText plans request and renders the plan in the TEXT form, returning
// the notes above its first step and each unit's statement.
func rebuiltText(c *qt.C, request *protocol.PlanFeaturesRequest) (notes, statements []string) {
	c.Helper()
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	for _, unit := range rendered.Units {
		statements = append(statements, unit.GetSql())
	}
	return plan.Contributions[0].Steps[0].Operation.Notes, statements
}

// retired is the drop of the old table of the rebuild of rebuildRequest.
const retired = "DROP TABLE `app/__ptah_replaced_records`;"

// A rebuild written into a migration file gives the new table, once it has
// the old table's name and the old table is gone, every grant the read found
// on the old table, except a privilege the table holds by default and a grant
// the plan revokes, whose REVOKE runs after the rebuild: YDB creates the new
// table with its directory's permissions alone and drops the old one with its
// own, and a migration file runs later and reads no server. The host's
// embedded engine gave the rebuilt table the same grants in the same place
// (G222, TEXT form; measured with built-in 74de0b65b on 26.2.1.14 through
// `migrations generate --allow-table-rebuild` and `migrations up`). The
// grants follow the drop, so the rebuild's steps stay consecutive for the
// migration lint, which reads them as one rebuild.
func TestRebuildPlanGivesBackTheGrants_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := grantedRebuild(c, &protocol.PlanningTableGrants{Complete: true, Grants: []*protocol.ObservedGrant{
		heldGrant("writers", "ydb.granular.update_row", false), heldGrant("readers", "ydb.granular.select_row", false),
		heldGrant("readers", "ydb.granular.erase_row", false), heldGrant("root", "ydb.generic.full", true),
	}})
	revoke := commonStep("revoke", nil, "")
	revoke.CommonObject = &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_Revoke{Revoke: &protocol.SQLGrant{Role: "readers",
		Privileges: []string{"ERASE ROW"}, Target: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}}}}
	request.CommonSteps = append(request.CommonSteps, revoke)

	notes, statements := rebuiltText(c, request)

	c.Assert(notes, qt.HasLen, 3)
	c.Assert(statements, qt.HasLen, 5)
	c.Assert(statements[4], qt.Equals, retired+"\n"+
		"GRANT 'ydb.granular.select_row' ON `app/records` TO `readers`;\n"+
		"GRANT 'ydb.granular.update_row' ON `app/records` TO `writers`;")
}

// A read that did not describe the grants leaves them unknown: the rebuild
// grants nothing, and a note above it says that a migration file
// gives the new table none, while a rebuild Ptah runs copies them from the old
// table. A host that sends no grants plans the copy alone, as
// TestRebuildPlan_HappyPath shows.
func TestRebuildPlanGivesBackTheGrantsTheReadDescribed_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	notes, statements := rebuiltText(c, grantedRebuild(c, &protocol.PlanningTableGrants{}))

	c.Assert(notes, qt.HasLen, 4)
	c.Assert(notes[3], qt.Equals, "The read did not describe the grants of table app/records, so the rebuild written into a migration file gives the new table "+
		"none of them; run by Ptah, the rebuild copies them from the old table.")
	c.Assert(statements, qt.HasLen, 5)
	c.Assert(statements[4], qt.Equals, retired)
}
