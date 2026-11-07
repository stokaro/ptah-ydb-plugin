package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// addedColumnRebuild is rebuildRequest asked to rebuild, whose declaration
// also adds the Utf8 column m, NOT NULL, with the default given, empty for
// none.
func addedColumnRebuild(c *qt.C, defaultValue string) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	request.Tables[0].Desired.Columns = append(request.Tables[0].Desired.Columns,
		&protocol.DeclaredColumn{Name: "m", Type: "Utf8", DefaultSet: defaultValue != "", DefaultValue: defaultValue})
	return request
}

// A column the rebuild adds NOT NULL takes its default in the new table: the
// copy leaves it out. Ported from the host's
// TestGenerateMigrationAST_TableRebuild_FailurePath, whose refusal this is the
// control of.
func TestRebuildPlanAddsANotNullColumn_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := addedColumnRebuild(c, "none")

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	statements := renderedText(c, plan, request.Capabilities)
	c.Assert(statements[0], qt.Contains, "\n    `m` Utf8 NOT NULL DEFAULT 'none'u,\n")
	c.Assert(rebuildStatementHeads(statements[1:2]), qt.DeepEquals, []string{"INSERT INTO `app/__ptah_rebuild_records`"})
	c.Assert(statements[1], qt.Not(qt.Contains), "`m`")
}

// A rebuild that adds a NOT NULL column without a default is refused before
// any statement: the copy has no value for it, and YDB refuses a NULL there.
// Ported from the host's TestGenerateMigrationAST_TableRebuild_FailurePath.
func TestRebuildPlanAddsANotNullColumn_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	plan, err := backend.New().PlanFeatures(c.Context(), addedColumnRebuild(c, ""))

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Contributions, qt.HasLen, 0)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, `rebuilding table "app/records": the copy has no value for column "m", which the new table adds NOT NULL `+
		`without a default: give it a default or make it nullable`)
}

// A rebuild driven by a declaration that states no family, as an HCL
// document is, keeps the families the old table holds and the family each
// kept column sits in. A column the plan drops in the same rebuild, which sat
// in one of those families, is left out of the new table and of the copy, and
// the host's step that drops it is rewritten to the rename that puts the new
// table in place, which names the column. Ported from the host's
// TestGenerateMigrationAST_ColumnFamilies_RebuildLeavesADroppedColumnOut; its
// refusal of a declared family naming a column the table does not declare is
// TestAlterPlan_FailurePath's.
func TestRebuildPlanLeavesADroppedColumnOutOfItsFamily(t *testing.T) {
	c := testcheck.New(t)
	request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	table := request.Tables[0]
	table.Current.Columns = append(table.Current.Columns, &protocol.ObservedColumn{Name: "gone", DataType: "Utf8", IsNullable: "YES"})
	held := alterRequest(c, alterCase{heldTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"cold","compression":"COMPRESSION_LZ4"}]}`,
		heldColumns: map[string]string{"n": `{"family":"cold"}`, "gone": `{"family":"cold"}`}})
	table.Current.Features = held.Tables[0].Current.Features
	gone := proto.CloneOf(table.Subject)
	gone.Kind, gone.Parent, gone.Name = "column", proto.CloneOf(table.Subject.Name), part("gone")
	request.CommonSteps = append(request.CommonSteps, commonStep("drop-gone", table.Subject, "",
		&protocol.PlanningEffect{Subject: proto.CloneOf(table.Subject), Action: protocol.PlanningEffect_READ},
		&protocol.PlanningEffect{Subject: gone, Action: protocol.PlanningEffect_DROP}))

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(uncoveredEffects(request, plan), qt.HasLen, 0)
	statements := renderedText(c, plan, request.Capabilities)
	c.Assert(statements[0], qt.Equals, "CREATE TABLE `app/__ptah_rebuild_records` (\n    `id` Uint64 NOT NULL,\n    `payload` Utf8,\n    `n` Int64 FAMILY `cold`,\n"+
		"    PRIMARY KEY (`id`),\n    FAMILY `cold` (COMPRESSION = 'lz4')\n) WITH (KEY_BLOOM_FILTER = DISABLED);")
	c.Assert(statements[1], qt.Not(qt.Contains), "gone")
}
