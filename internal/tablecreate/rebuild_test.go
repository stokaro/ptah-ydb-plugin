package tablecreate_test

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/google/go-cmp/cmp/cmpopts"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
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
	check := testcheck.New(t)
	key := facet(check, schema.TableProperties, "", `{"primaryKey":["id","payload"]}`)
	for _, tc := range []struct {
		name   string
		modify func(*protocol.PlanFeaturesRequest)
		want   string
	}{
		{name: "a column made NOT NULL", modify: func(r *protocol.PlanFeaturesRequest) {
			r.Tables[0].Desired.Columns[1].Nullable, r.Tables[0].Desired.Columns[2].Type = false, "Int32"
		},
			want: `making column "payload" of table "app/records" NOT NULL, which requires target capability alter_column_set_not_null, unavailable on this ydb target; ` +
				`YDB makes it by rebuilding the table, which Ptah plans when asked with --allow-table-rebuild`},
		{name: "the key", modify: func(r *protocol.PlanFeaturesRequest) {
			r.Tables[0].Desired.Columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "payload", Type: "Utf8", Primary: true},
				{Name: "n", Type: "Int32", Nullable: true}}
			r.Tables[0].Desired.PrimaryKey = []string{"id", "payload"}
			r.Tables[0].Desired.Features.Facets.Records = []*protocol.ComparisonRecord{key}
		}, want: `changing the primary key of table "app/records" (id -> id, payload), which requires target capability primary_key_alterable, unavailable on this ydb target; ` +
			`YDB makes it by rebuilding the table, which Ptah plans when asked with --allow-table-rebuild`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := testcheck.New(t)
			request := rebuildRequest(check, &protocol.PlanningPolicy{TableRebuildRequest: "--allow-table-rebuild"})
			tc.modify(request)
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

// heldUpdates is the changefeed updates a read finds on app.records, with
// its consumer audit.
const heldUpdates = `{"changefeeds":[{"description":{"name":"updates","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},` +
	`"topic":{"partitioningSettings":{},"consumers":[{"name":"audit","important":true}]}}]}`

// feedRebuildRequest is rebuildRequest on a table that holds changefeeds held,
// whose declaration states desired, none for empty, on a target with
// capabilities.
func feedRebuildRequest(c *qt.C, desired, held string, capabilities map[string]bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	request.Tables[0].Desired.Columns[1].Nullable = false
	table := request.Tables[0]
	table.Current.Features.Facets.Records = append(table.Current.Features.Facets.Records, observedFacet(c, schema.ChangefeedProperties, "", held))
	if desired != "" {
		table.Desired.Features.Facets.Records = append(table.Desired.Features.Facets.Records, facet(c, schema.ChangefeedProperties, "", desired))
	}
	maps.Copy(request.Capabilities, capabilities)
	return request
}

// YDB moves no table that carries a changefeed, so a rebuild drops the old
// table's changefeeds before it moves and gives the new table its changefeeds
// once it is in place: the ones the declaration states, or the ones the old
// table held where the declaration states none. The note says the stream
// restarts and the consumer loses its position.
func TestRebuildPlanCarriesChangefeeds_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, desired string
		added         []string
	}{
		{name: "the changefeeds the old table held", added: []string{"ALTER TABLE `app/records` ADD CHANGEFEED `updates` WITH (MODE = 'UPDATES', FORMAT = 'JSON');",
			"ALTER TOPIC `app/records/updates` ADD CONSUMER `audit` WITH (important = TRUE);"}},
		{name: "the changefeeds the declaration states",
			desired: `{"changefeeds":[{"name":"inserts","mode":"MODE_NEW_IMAGE","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`,
			added:   []string{"ALTER TABLE `app/records` ADD CHANGEFEED `inserts` WITH (MODE = 'NEW_IMAGE', FORMAT = 'JSON');"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := testcheck.New(t)
			request := feedRebuildRequest(check, test.desired, heldUpdates, newest())
			plan, err := backend.New().PlanFeatures(check.Context(), request)
			check.Assert(err, qt.IsNil)
			check.Assert(plan.Diagnostics, qt.HasLen, 0)
			check.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			var names []string
			for _, step := range plan.Contributions[0].Steps {
				names = append(names, step.Id.Name)
			}
			check.Assert(names, qt.DeepEquals, []string{"table-rebuild/000000/create", "table-rebuild/000000/copy", "table-rebuild/000000/drop-changefeeds",
				"table-rebuild/000000/rename-old", "table-rebuild/000000/rename-new", "table-rebuild/000000/add-changefeeds", "table-rebuild/000000/drop-old"})
			check.Assert(plan.Contributions[0].Dependencies, qt.HasLen, 6)
			check.Assert(plan.Rewrites[0].Replacement.Name, qt.Equals, "table-rebuild/000000/rename-new")
			check.Assert(plan.Contributions[0].Steps[2].Operation.Notes, qt.DeepEquals, []string{"Changefeed updates of table app/records is dropped " +
				"before the swap, because YDB moves no table that carries one, and added again to the new table. Its stream restarts: the records " +
				"nobody read are lost, and consumer audit loses its position and starts again from the beginning of the new stream."})

			dropped, err := schema.DecodeChangefeedOperation(plan.Contributions[0].Steps[2].Operation.Value.Value)
			check.Assert(err, qt.IsNil)
			drops, err := changefeeds.Statements("app/records", dropped)
			check.Assert(err, qt.IsNil)
			check.Assert(drops, qt.DeepEquals, []string{"ALTER TABLE `app/records` DROP CHANGEFEED `updates`;"})
			added, err := schema.DecodeChangefeedOperation(plan.Contributions[0].Steps[5].Operation.Value.Value)
			check.Assert(err, qt.IsNil)
			adds, err := changefeeds.Statements("app/records", added)
			check.Assert(err, qt.IsNil)
			check.Assert(adds, qt.DeepEquals, test.added)
		})
	}
}

// A changefeed the rebuild cannot drop and add again refuses the rebuild: one
// an asynchronous replication controls, which the old table would still carry
// when it moves, and any changefeed on a target that has none.
func TestRebuildPlanCarriesChangefeeds_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, held   string
		capabilities map[string]bool
		want         string
	}{
		{name: "a changefeed a replication controls", capabilities: newest(),
			held: `{"changefeeds":[{"description":{"name":"updates","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED",` +
				`"attributes":{"__async_replication":"{\"path\":\"/local/r\",\"id\":\"1\",\"supports_topic_autopartitioning\":false}"}},"topic":{"partitioningSettings":{}}}]}`,
			want: `rebuilding table "app/records": the table holds a changefeed an asynchronous replication controls, and YDB moves no table that ` +
				"carries one \\(`Cannot move table with cdc streams`\\); the rebuild cannot drop it and add it again"},
		{name: "a target without changefeeds", held: heldUpdates,
			want: `rebuilding table "app/records": changefeed "updates" of table "app/records", which requires target capability changefeeds, unavailable on this ydb target`},
	} {
		t.Run(test.name, func(t *testing.T) {
			check := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(check.Context(), feedRebuildRequest(check, "", test.held, test.capabilities))
			check.Assert(err, qt.IsNil)
			check.Assert(plan.Contributions, qt.HasLen, 0)
			check.Assert(plan.Diagnostics, qt.HasLen, 1)
			check.Assert(plan.Diagnostics[0].Message, qt.Matches, test.want)
		})
	}
}

// The read reports the table's comments on the observed table, its columns
// and its indexes, apart from its properties. The new table of a rebuild keeps
// them, those of the columns it has, and a migration file writes them after its
// CREATE TABLE. The comment of a column the new table leaves out is not
// written. The host fills no observed common objects for a table, so a rebuild
// that read its comments there kept none.
func TestRebuildPlanKeepsTheComments(t *testing.T) {
	check := testcheck.New(t)
	request := rebuildRequest(check, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	request.TargetFeatures = append(request.TargetFeatures, provider.CommonObjectsFeature)
	current := request.Tables[0].Current
	current.Comment = "rows"
	current.Columns[1].Comment = "the text"
	current.Columns = append(current.Columns, &protocol.ObservedColumn{Name: "gone", DataType: "Utf8", IsNullable: "YES", Comment: "dropped"})
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
		{"a declared vector index", []string{declaredVectorIndexes()}, nil},
		{"a held vector index", nil, []string{heldVectorIndexes()}},
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

// heldPayloadIndex is the index set of a table holding a synchronous index on
// payload under name, with the settings fields given.
func heldPayloadIndex(name, settings string) string {
	return `{"indexes":[{"description":{"name":"` + name + `","indexColumns":["payload"],"globalIndex":{"settings":{` + settings + `}},"status":"STATUS_READY"},` +
		`"tables":[{"name":"indexImplTable","description":{"columns":[{"name":"payload","type":{"typeId":"UTF8"}},{"name":"id","type":{"typeId":"UINT64"}}],` +
		`"primaryKey":["payload","id"]}}]}]}`
}

// payloadIndexSubject names index name of table.
func payloadIndexSubject(table *protocol.ComparisonIdentity, name string) *protocol.ComparisonIdentity {
	subject := proto.CloneOf(table)
	subject.Kind, subject.Parent, subject.Name = "index", proto.CloneOf(table.Name), part(name)
	return subject
}

// The native call that creates the new table of a rebuild takes no index
// settings, and the declaration states none for an index it takes from the
// common declaration. So each index whose held partitioning or read replicas
// differ from a new index's keeps them through an index settings change on
// the new table, before the copy fills it, naming every setting, as the
// host's embedded engine names every setting of a rebuilt index. The old
// index is the one under the same name, or under the name the host renames
// it from.
func TestRebuildPlanKeepsTheIndexSettingsTheTableHolds(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared string
		held     string
		renamed  bool
		// statements are the units the plan writes between the creation
		// and the copy.
		statements []string
	}{
		{name: "an index held with its own partitioning", declared: "by_payload",
			held: heldPayloadIndex("by_payload", `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"2"}`),
			statements: []string{"ALTER TABLE `app/__ptah_rebuild_records` ALTER INDEX `by_payload` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, " +
				"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);"}},
		{name: "an index that splits by size no more, with replicas and a maximum", declared: "by_payload",
			held: heldPayloadIndex("by_payload", `"partitioningSettings":{"partitioningBySize":"DISABLED","partitioningByLoad":"DISABLED","minPartitionsCount":"1",`+
				`"maxPartitionsCount":"9","partitionBy":["payload"]},"readReplicasSettings":{"anyAzReadReplicasCount":"1"}`),
			statements: []string{"ALTER TABLE `app/__ptah_rebuild_records` ALTER INDEX `by_payload` SET (AUTO_PARTITIONING_BY_SIZE = DISABLED, " +
				"AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1, AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 9, " +
				`READ_REPLICAS_SETTINGS = "ANY_AZ:1");`}},
		{name: "an index the common model describes, which the read describes beside the native set", declared: "by_payload",
			held: `{"commonIndexes":[{"name":"by_payload","indexColumns":["payload"],"globalIndex":{"settings":{"partitioningSettings":{"partitioningBySize":"ENABLED",` +
				`"partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"2"}}}}]}`,
			statements: []string{"ALTER TABLE `app/__ptah_rebuild_records` ALTER INDEX `by_payload` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, " +
				"AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);"}},
		{name: "an index the host renames", declared: "by_text", renamed: true,
			held: heldPayloadIndex("by_payload", `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"512","partitioningByLoad":"DISABLED","minPartitionsCount":"3"}`),
			statements: []string{"ALTER TABLE `app/__ptah_rebuild_records` ALTER INDEX `by_text` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, " +
				"AUTO_PARTITIONING_PARTITION_SIZE_MB = 512, AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);"}},
		{name: "an index held with a new index's settings", declared: "by_payload", held: heldPayloadIndex("by_payload", heldIndexDefaults)},
		{name: "an index the old table does not hold", declared: "by_text",
			held: heldPayloadIndex("by_payload", `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"2"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
			table := request.Tables[0]
			table.Desired.Indexes = []*protocol.DeclaredIndex{{Name: test.declared, Fields: []string{"payload"}}}
			table.Current.Features.Facets.Records = append(table.Current.Features.Facets.Records, observedFacet(c, schema.IndexProperties, "", test.held))
			table.Current.Indexes = []*protocol.ObservedIndex{{Name: "by_payload", TableName: "records", Schema: "app", Columns: []string{"payload"}}}
			if test.renamed {
				rename := commonStep("rename-index", table.Subject, "",
					&protocol.PlanningEffect{Subject: proto.CloneOf(table.Subject), Action: protocol.PlanningEffect_READ},
					&protocol.PlanningEffect{Subject: payloadIndexSubject(table.Subject, "by_payload"), Action: protocol.PlanningEffect_DROP},
					&protocol.PlanningEffect{Subject: payloadIndexSubject(table.Subject, test.declared), Action: protocol.PlanningEffect_CREATE})
				rename.RenamedIndex = &protocol.CommonIndexRename{From: "by_payload", To: test.declared}
				request.CommonSteps = append(request.CommonSteps, rename)
				request.TargetFeatures = append(request.TargetFeatures, provider.IndexRenamesFeature)
				request.Capabilities[tablecreate.IndexRenameCapability] = true
			}
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			c.Assert(plan.Rewrites, qt.HasLen, 1)
			c.Assert(plan.Rewrites[0].Replacement.Name, qt.Equals, "table-rebuild/000000/rename-new")
			c.Assert(plan.Contributions[0].Dependencies, qt.HasLen, len(plan.Contributions[0].Steps)-1)

			rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
			rendering.Form = protocol.RenderRequest_TEXT
			rendered, err := backend.New().Render(c.Context(), rendering)
			c.Assert(err, qt.IsNil)
			var statements []string
			for _, unit := range rendered.Units {
				statements = append(statements, unit.GetSql())
			}
			c.Assert(statements, qt.HasLen, 5+len(test.statements))
			c.Assert(statements[0], qt.Matches, "(?s)CREATE TABLE `app/__ptah_rebuild_records` .*")
			c.Assert(statements[1:1+len(test.statements)], qt.CmpEquals(cmpopts.EquateEmpty()), test.statements)
			c.Assert(statements[1+len(test.statements)], qt.Matches, "INSERT INTO `app/__ptah_rebuild_records` .*")
		})
	}
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

// layoutRebuildRequest alters app.records, which holds partitioning by load
// and a minimum of two partitions, into a declaration of the same columns
// with the starting layout layout, which only a creation takes.
func layoutRebuildRequest(c *qt.C, policy *protocol.PlanningPolicy, layout string) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := rebuildRequest(c, policy)
	table := request.Tables[0]
	table.Desired.Columns[2].Type = "Int32"
	table.Desired.Features.Facets.Records = []*protocol.ComparisonRecord{facet(c, schema.TableProperties, "", `{"primaryKey":["id"],`+layout+`}`)}
	request.CommonSteps = nil
	return request
}

// Asked for a rebuild, a starting layout the table was not created with is
// planned as one: the new table is created with the layout and the held value
// of every setting the declaration leaves out but the minimum, which the
// layout sets.
func TestRebuildPlanCreatesAStartingLayout_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name, layout, want string
	}{
		{"uniform partitions", `"uniformPartitions":"4"`, "UNIFORM_PARTITIONS = 4"},
		{"split points", `"partitionAtKeys":[{"values":["10"]},{"values":["20"]}]`, "PARTITION_AT_KEYS = ((10), (20))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := testcheck.New(t)
			request := layoutRebuildRequest(check, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"}, tc.layout)
			plan, err := backend.New().PlanFeatures(check.Context(), request)
			check.Assert(err, qt.IsNil)
			check.Assert(plan.Diagnostics, qt.HasLen, 0)
			check.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
			check.Assert(plan.Contributions[0].Steps, qt.HasLen, 5)
			rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
			rendering.Form = protocol.RenderRequest_TEXT
			rendered, err := backend.New().Render(check.Context(), rendering)
			check.Assert(err, qt.IsNil)
			check.Assert(rendered.Units[0].GetSql(), qt.Equals, "CREATE TABLE `app/__ptah_rebuild_records` (\n    `id` Uint64 NOT NULL,\n    `payload` Utf8,\n    `n` Int32,\n"+
				"    PRIMARY KEY (`id`)\n) WITH (AUTO_PARTITIONING_BY_LOAD = ENABLED, "+tc.want+");")
		})
	}
}

// Without a rebuild in the policy the layout is refused with the embedded
// engine's words, naming how to ask for the rebuild.
func TestRebuildPlanCreatesAStartingLayout_FailurePath(t *testing.T) {
	check := testcheck.New(t)
	plan, err := backend.New().PlanFeatures(check.Context(), layoutRebuildRequest(check, &protocol.PlanningPolicy{TableRebuildRequest: "--allow-table-rebuild"}, `"uniformPartitions":"4"`))
	check.Assert(err, qt.IsNil)
	check.Assert(plan.Diagnostics, qt.HasLen, 1)
	check.Assert(plan.Diagnostics[0].Message, qt.Equals, "table \"app/records\": it declares UNIFORM_PARTITIONS = 4, which YDB takes only when it creates a table "+
		"(`UNIFORM_PARTITIONS alter is not supported`) and which gives a new table a minimum of 4 partitions, where this table holds 2. Declare "+
		"auto_partitioning_min_partitions_count to change the minimum in place; YDB makes it by rebuilding the table, which Ptah plans when asked with --allow-table-rebuild")
	check.Assert(plan.Contributions, qt.HasLen, 0)
}

// ttlRebuildRequest is rebuildRequest on a table that also holds a Timestamp
// column ts and a TTL on it after a day, run on an interval of half an hour,
// whose declaration states a TTL on ts after seconds.
func ttlRebuildRequest(c *qt.C, seconds int) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	table := request.Tables[0]
	table.Desired.Columns = append(table.Desired.Columns, &protocol.DeclaredColumn{Name: "ts", Type: "Timestamp", Nullable: true})
	table.Current.Columns = append(table.Current.Columns, &protocol.ObservedColumn{Name: "ts", DataType: "Timestamp", IsNullable: "YES"})
	held := alterRequest(c, alterCase{heldTable: `{"primaryKey":["id"],"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400},"runIntervalSeconds":1800}}`})
	table.Current.Features = held.Tables[0].Current.Features
	table.Desired.Features.Facets.Records = []*protocol.ComparisonRecord{facet(c, schema.TableProperties, "",
		fmt.Sprintf(`{"primaryKey":["id"],"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":%d}}}`, seconds))}
	return request
}

// A declared TTL names no run interval, so a table rebuilt with the TTL its
// declaration states keeps the run interval the old table holds, whether the
// declaration keeps the held TTL or changes it, as a TTL changed in place
// does. Without it the new table would run its TTL on YDB's default, and the
// next plan would find nothing to change.
func TestRebuildPlanKeepsTheHeldTTLRunInterval_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seconds int
	}{
		{name: "the held TTL", seconds: 86400},
		{name: "another TTL", seconds: 259200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(check.Context(), ttlRebuildRequest(check, tc.seconds))
			check.Assert(err, qt.IsNil)
			check.Assert(plan.Diagnostics, qt.HasLen, 0)

			created, err := tablecreate.Request(plan.Contributions[0].Steps[0].Operation.Value.Value)

			check.Assert(err, qt.IsNil)
			check.Assert(created.GetTtlSettings().GetDateTypeColumn().GetColumnName(), qt.Equals, "ts")
			check.Assert(created.GetTtlSettings().GetDateTypeColumn().GetExpireAfterSeconds(), qt.Equals, uint32(tc.seconds)) // #nosec G115 -- the rows hold a day and three days in seconds
			check.Assert(created.GetTtlSettings().GetRunIntervalSeconds(), qt.Equals, uint32(1800))
		})
	}
}

// YQL's TTL setting carries no run interval, so the text form of a rebuild
// that keeps one is refused at the new table: a migration file cannot drop
// the interval the table holds.
func TestRebuildPlanKeepsTheHeldTTLRunInterval_FailurePath(t *testing.T) {
	check := testcheck.New(t)
	request := ttlRebuildRequest(check, 259200)
	plan, err := backend.New().PlanFeatures(check.Context(), request)
	check.Assert(err, qt.IsNil)
	check.Assert(plan.Diagnostics, qt.HasLen, 0)
	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT

	rendered, err := backend.New().Render(check.Context(), rendering)

	check.Assert(err, qt.IsNil)
	check.Assert(rendered.GetUnits(), qt.HasLen, 0)
	check.Assert(rendered.GetRefusal().GetChangeId(), qt.Equals, plan.Contributions[0].Steps[0].Operation.Value.Id)
	check.Assert(rendered.GetRefusal().GetReason(), qt.Equals, protocol.RenderRefusal_NO_TEXT_FORM)
	check.Assert(rendered.GetRefusal().GetMessage(), qt.Equals, "no YQL text form: the TTL runs on an interval of its own, and YQL's TTL setting has none")
}
