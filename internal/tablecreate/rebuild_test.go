package tablecreate_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// rebuildRequest alters app.records, which holds id Uint64, payload Utf8 and
// n Int32, into a declaration of n as Int64, with the host's step for the
// column change.
func rebuildRequest(c *qt.C, policy *protocol.PlanningPolicy) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := alterRequest(c, alterCase{heldTable: `{"primaryKey":["id"],"partitioningSettings":{"partitioningByLoad":"ENABLED","minPartitionsCount":"2"}}`})
	table := request.Tables[0]
	table.Desired.Columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "payload", Type: "Utf8", Nullable: true},
		{Name: "n", Type: "Int64", Nullable: true}}
	table.Current.Columns = []*protocol.ObservedColumn{{Name: "id", DataType: "Uint64", IsNullable: "YES", PrimaryKey: true},
		{Name: "payload", DataType: "Utf8", IsNullable: "YES"}, {Name: "n", DataType: "Int32", IsNullable: "YES"}}
	column := proto.CloneOf(table.Subject)
	column.Kind, column.Parent, column.Name = "column", proto.CloneOf(table.Subject.Name), part("n")
	request.CommonSteps = []*protocol.CommonPlanningStep{commonStep("modify-n", table.Subject, "",
		&protocol.PlanningEffect{Subject: proto.CloneOf(table.Subject), Action: protocol.PlanningEffect_READ},
		&protocol.PlanningEffect{Subject: column, Action: protocol.PlanningEffect_ALTER})}
	request.Policy = policy
	if policy != nil {
		request.TargetFeatures = append(request.TargetFeatures, provider.PlanningPolicyFeature)
	}
	return request
}

// Without a rebuild in the policy a change YDB makes only by rebuilding is
// refused with the embedded engine's words, naming how the operator asks for
// one.
func TestRebuildPlan_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy *protocol.PlanningPolicy
		want   string
	}{
		{name: "a policy without a rebuild", policy: &protocol.PlanningPolicy{TableRebuildRequest: "--allow-table-rebuild"},
			want: `changing the type of column "n" of table "app/records" (Int32 -> Int64), which requires target capability alter_column_type, ` +
				`unavailable on this ydb target; YDB makes it by rebuilding the table, which Ptah plans when asked with --allow-table-rebuild`},
		{name: "the compatibility surface's request", policy: &protocol.PlanningPolicy{TableRebuildRequest: "PTAH_ALLOW_TABLE_REBUILD=1"},
			want: `changing the type of column "n" of table "app/records" (Int32 -> Int64), which requires target capability alter_column_type, ` +
				`unavailable on this ydb target; YDB makes it by rebuilding the table, which Ptah plans when asked with PTAH_ALLOW_TABLE_REBUILD=1`},
		{name: "a host of an earlier minor", want: `changing the type of column "n" of table "app/records" (Int32 -> Int64), which requires target capability ` +
			`alter_column_type, unavailable on this ydb target; YDB makes it by rebuilding the table, which Ptah plans when asked with a table rebuild request ` +
			`in the host's planning policy (contract 25.3)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), rebuildRequest(c, tc.policy))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, tc.want)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

func TestRebuildPlanNamesEachChange(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*qt.C, *protocol.PlanFeaturesRequest)
		want   string
	}{
		{name: "a column made NOT NULL", modify: func(_ *qt.C, r *protocol.PlanFeaturesRequest) {
			r.Tables[0].Desired.Columns[1].Nullable, r.Tables[0].Desired.Columns[2].Type = false, "Int32"
		},
			want: `making column "payload" of table "app/records" NOT NULL, which requires target capability alter_column_set_not_null, unavailable on this ydb target; ` +
				`YDB makes it by rebuilding the table, which Ptah plans when asked with --allow-table-rebuild`},
		{name: "the key", modify: func(c *qt.C, r *protocol.PlanFeaturesRequest) {
			r.Tables[0].Desired.Columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "payload", Type: "Utf8", Primary: true},
				{Name: "n", Type: "Int32", Nullable: true}}
			r.Tables[0].Desired.PrimaryKey = []string{"id", "payload"}
			r.Tables[0].Desired.Features.Facets.Records = []*protocol.ComparisonRecord{facet(c, schema.TableProperties, "", `{"primaryKey":["id","payload"]}`)}
		}, want: `changing the primary key of table "app/records" (id -> id, payload), which requires target capability primary_key_alterable, unavailable on this ydb target; ` +
			`YDB makes it by rebuilding the table, which Ptah plans when asked with --allow-table-rebuild`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := testcheck.New(t)
			request := rebuildRequest(check, &protocol.PlanningPolicy{TableRebuildRequest: "--allow-table-rebuild"})
			tc.modify(check, request)
			plan, err := backend.New().PlanFeatures(check.Context(), request)
			check.Assert(err, qt.IsNil)
			check.Assert(plan.Diagnostics, qt.HasLen, 1)
			check.Assert(plan.Diagnostics[0].Message, qt.Equals, tc.want)
		})
	}
}

// With a rebuild in the policy, the change is planned as five steps in order:
// the new table, the copy, the two renames and the removal of the old table.
// The host's step for the column becomes the step that puts the new table in
// place, and every step writes as the statement a migration file holds.
func TestRebuildPlan_HappyPath(t *testing.T) {
	check := testcheck.New(t)
	request := rebuildRequest(check, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	request.Tables[0].Desired.Columns[1].Nullable = false
	plan, err := backend.New().PlanFeatures(check.Context(), request)
	check.Assert(err, qt.IsNil)
	check.Assert(plan.Diagnostics, qt.HasLen, 0)
	check.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	steps := plan.Contributions[0].Steps
	var names, kinds []string
	for _, step := range steps {
		check.Assert(step.Operation.Phase, qt.Equals, protocol.PlanningOperation_LIFECYCLE)
		names = append(names, step.Id.Name)
		kinds = append(kinds, step.Operation.Value.Value.Kind)
	}
	check.Assert(names, qt.DeepEquals, []string{"table-rebuild/000000/create", "table-rebuild/000000/copy", "table-rebuild/000000/rename-old",
		"table-rebuild/000000/rename-new", "table-rebuild/000000/drop-old"})
	check.Assert(kinds, qt.DeepEquals, []string{tablecreate.OperationKind, tablecreate.CopyOperationKind, tablecreate.RenameOperationKind,
		tablecreate.RenameOperationKind, tablecreate.DropOperationKind})
	check.Assert(plan.Contributions[0].Dependencies, qt.HasLen, 4)
	check.Assert(steps[0].Operation.Notes, qt.HasLen, 3)
	check.Assert(plan.Rewrites, qt.HasLen, 1)
	check.Assert(plan.Rewrites[0].Sources[0].Name, qt.Equals, "modify-n")
	check.Assert(plan.Rewrites[0].Replacement.Name, qt.Equals, "table-rebuild/000000/rename-new")
	check.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 5)

	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(check.Context(), rendering)
	check.Assert(err, qt.IsNil)
	var statements []string
	for _, unit := range rendered.Units {
		statements = append(statements, unit.GetSql())
	}
	check.Assert(statements, qt.DeepEquals, []string{
		"CREATE TABLE `app/__ptah_rebuild_records` (\n    `id` Uint64 NOT NULL,\n    `payload` Utf8 NOT NULL,\n    `n` Int64,\n    PRIMARY KEY (`id`)\n) " +
			"WITH (AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);",
		"INSERT INTO `app/__ptah_rebuild_records` (`id`, `payload`, `n`) SELECT " +
			"Unwrap(`id`, 'rebuilding table app.records: column id holds NULL, and the new table declares it NOT NULL'u) AS `id`, " +
			"Unwrap(`payload`, 'rebuilding table app.records: column payload holds NULL, and the new table declares it NOT NULL'u) AS `payload`, " +
			"IF(`n` IS NULL, NULL, Unwrap(CAST(`n` AS Int64), 'rebuilding table app.records: column n holds a value that does not convert to its new type'u)) AS `n` " +
			"FROM `app/records`;",
		"ALTER TABLE `app/records` RENAME TO `app/__ptah_replaced_records`;",
		"ALTER TABLE `app/__ptah_rebuild_records` RENAME TO `app/records`;",
		"DROP TABLE `app/__ptah_replaced_records`;",
	})
}

// A table holding changefeeds is not rebuilt: YDB moves no table with one.
func TestRebuildPlanRefusesChangefeeds(t *testing.T) {
	check := testcheck.New(t)
	request := rebuildRequest(check, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	request.Tables[0].Current.Features.Facets.Records = append(request.Tables[0].Current.Features.Facets.Records,
		observedFacet(check, schema.ChangefeedProperties, "",
			`{"changefeeds":[{"description":{"name":"updates","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{}}}]}`))
	plan, err := backend.New().PlanFeatures(check.Context(), request)
	check.Assert(err, qt.IsNil)
	check.Assert(plan.Diagnostics, qt.HasLen, 1)
	check.Assert(plan.Diagnostics[0].Message, qt.Matches, `rebuilding table "app/records": the table holds changefeeds, .*`)
}

// On the common wire a read reports the table's comments apart from its
// properties. The new table of a rebuild keeps them, the comments of the
// columns it has, and a migration file writes them after its CREATE TABLE.
func TestRebuildPlanKeepsTheComments(t *testing.T) {
	check := testcheck.New(t)
	request := rebuildRequest(check, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	request.TargetFeatures = append(request.TargetFeatures, provider.CommonObjectsFeature)
	comment := func(kind, name, text string, path ...string) *protocol.ObjectComment {
		return &protocol.ObjectComment{Object: &protocol.Reference{Engine: "ydb", Kind: kind, Schema: "app", Name: name, Path: path}, Text: text}
	}
	request.CurrentCommon = &protocol.ObservedCommonObjects{
		Coverage: []*protocol.CommonObjectCoverage{{Kind: protocol.CommonObjectSupport_VIEW, Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}},
			{Kind: protocol.CommonObjectSupport_COMMENT, Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}},
			{Kind: protocol.CommonObjectSupport_ROLE, Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "The fixture did not inspect principals"}},
			{Kind: protocol.CommonObjectSupport_GRANT, Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "The fixture did not inspect grants"}}},
		Comments: []*protocol.ObjectComment{comment("table", "records", "rows"), comment("column", "payload", "the text", "records"),
			comment("column", "gone", "dropped", "records"), comment("table", "other", "another table")},
	}
	plan, err := backend.New().PlanFeatures(check.Context(), request)
	check.Assert(err, qt.IsNil)
	check.Assert(plan.Diagnostics, qt.HasLen, 0)
	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(check.Context(), rendering)
	check.Assert(err, qt.IsNil)
	check.Assert(rendered.Units[0].GetSql(), qt.Contains, ";\nCOMMENT ON TABLE `app/__ptah_rebuild_records` IS 'rows';\n"+
		"COMMENT ON COLUMN `app/__ptah_rebuild_records`.`payload` IS 'the text';")
	check.Assert(strings.HasSuffix(rendered.Units[0].GetSql(), "'the text';"), qt.IsTrue)
}

// A table that declares or holds a vector or full-text index is not rebuilt:
// the new table is created with the global indexes of its declaration alone,
// and the index would be lost.
func TestRebuildPlanRefusesVectorAndFullTextIndexes(t *testing.T) {
	for _, test := range []struct {
		name          string
		desired, held []string
	}{
		{"a declared vector index", []string{vectorIndexes(false)}, nil},
		{"a held vector index", nil, []string{vectorIndexes(true)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
			table := request.Tables[0]
			for _, payload := range test.desired {
				table.Desired.Features.Facets.Records = append(table.Desired.Features.Facets.Records, facet(c, schema.IndexProperties, "", payload))
			}
			for _, payload := range test.held {
				table.Current.Features.Facets.Records = append(table.Current.Features.Facets.Records, observedFacet(c, schema.IndexProperties, "", payload))
			}
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, `rebuilding table "app/records": the table declares or holds the vector or full-text index "by_embedding", which the rebuild does not create; .*`)
		})
	}
}

// A rebuild writes into the new table every family the old one holds, the
// default family's own settings and a family no column sits in included,
// and places each column where the old table holds it, when the
// declaration states none of them: a cluster's table profile may have given
// them, and YQL drops neither a family nor a setting. Without that the new
// table would lose the default family's compression and the empty family.
func TestRebuildPlanKeepsTheFamiliesTheTableHolds(t *testing.T) {
	check := testcheck.New(t)
	request := rebuildRequest(check, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	held := alterRequest(check, alterCase{heldTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default","compression":"COMPRESSION_LZ4"},` +
		`{"name":"cold","data":{"media":"hdd"},"compression":"COMPRESSION_NONE"},{"name":"extra","compression":"COMPRESSION_LZ4"}]}`,
		heldColumns: map[string]string{"payload": `{"family":"cold"}`}, held: []string{"payload"}})
	request.Tables[0].Current.Features = held.Tables[0].Current.Features
	plan, err := backend.New().PlanFeatures(check.Context(), request)
	check.Assert(err, qt.IsNil)
	check.Assert(plan.Diagnostics, qt.HasLen, 0)

	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(check.Context(), rendering)

	check.Assert(err, qt.IsNil)
	check.Assert(rendered.Units[0].GetSql(), qt.Equals, "CREATE TABLE `app/__ptah_rebuild_records` (\n    `id` Uint64 NOT NULL,\n    `payload` Utf8 FAMILY `cold`,\n    `n` Int64,\n"+
		"    PRIMARY KEY (`id`),\n    FAMILY `cold` (DATA = 'hdd', COMPRESSION = 'off'),\n    FAMILY `default` (COMPRESSION = 'lz4'),\n    FAMILY `extra` (COMPRESSION = 'lz4')\n);")
}

// The new table of a rebuild puts a column where the declaration places it:
// in the family a column property record names, the default family for an
// empty one, and the default family for one a source that describes where
// every column sits places nowhere. Only a column a source could not describe
// keeps the family the old table holds it in (see
// TestRebuildPlanKeepsTheFamiliesTheTableHolds).
func TestRebuildPlanPlacesTheColumnsTheDeclarationPlaces(t *testing.T) {
	for _, test := range []struct {
		name string
		// placement is the column property record of payload, empty for none.
		placement string
		known     protocol.ModelKnowledge_State
	}{
		{name: "a column a source that describes every column places nowhere", known: protocol.ModelKnowledge_COMPLETE},
		{name: "a column the declaration places in the default family", placement: `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
			in := alterCase{heldTable: `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},{"name":"cold","compression":"COMPRESSION_LZ4"}]}`,
				heldColumns: map[string]string{"payload": `{"family":"cold"}`}, held: []string{"payload"}, columnKnowledge: test.known}
			if test.placement != "" {
				in.desiredColumns = map[string]string{"payload": test.placement}
			}
			fixture := alterRequest(c, in)
			request.Tables[0].Current.Features = fixture.Tables[0].Current.Features
			request.Tables[0].Desired.Features = fixture.Tables[0].Desired.Features
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)

			rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
			rendering.Form = protocol.RenderRequest_TEXT
			rendered, err := backend.New().Render(c.Context(), rendering)
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.Units[0].GetSql(), qt.Equals, "CREATE TABLE `app/__ptah_rebuild_records` (\n    `id` Uint64 NOT NULL,\n    `payload` Utf8,\n    `n` Int64,\n"+
				"    PRIMARY KEY (`id`),\n    FAMILY `cold` (COMPRESSION = 'lz4')\n);")
		})
	}
}
