package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// indexFacetRecord is the facet of index name of app.records in one
// representation, as the host sends an index's own facet: the subject names
// the index with the table as its parent.
func indexFacetRecord(c *qt.C, representation protocol.ValueInput_Representation, name, payload string) *protocol.ComparisonRecord {
	c.Helper()
	reference := &protocol.Reference{Engine: "ydb", Kind: "index", Database: "/local", Schema: "app", Path: []string{"records"}, Name: name}
	model := schema.NativeDesiredModel(schema.IndexSettingsProperties)
	if representation == protocol.ValueInput_OBSERVED {
		for _, candidate := range schema.ObservedModels() {
			if candidate.Kind == schema.IndexSettingsProperties {
				model = candidate
			}
		}
	}
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: "index." + name, Representation: representation, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.IndexSettingsProperties,
			SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: reference, Payload: []byte(payload)}},
	}})
	c.Assert(err, qt.IsNil)
	return &protocol.ComparisonRecord{Kind: schema.IndexSettingsProperties, Value: values.Values[0],
		Subject: &protocol.ComparisonIdentity{Kind: "index", Database: part("/local"), Schema: part("app"), Parent: part("records"), Name: part(name)}}
}

// heldDefaults is what a read reports for a global index that holds the
// settings YDB gives a new one.
const heldDefaults = `{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"}}`

// indexSettingsRequest alters app.records, which holds global index by_label
// on label, read by the host, with the index's own facet declared as declared
// and read as held; an empty text states no facet on that side.
func indexSettingsRequest(c *qt.C, declared, held string) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := alterRequest(c, alterCase{desiredTable: plainTable, heldTable: plainTable, declared: []string{"label"}, held: []string{"label"}})
	table := request.Tables[0]
	table.Desired.Indexes = []*protocol.DeclaredIndex{{Name: "by_label", Fields: []string{"label"}}}
	table.Current.Indexes = []*protocol.ObservedIndex{{Name: "by_label", TableName: "records", Schema: "app", Columns: []string{"label"}}}
	if declared != "" {
		table.Desired.Features.Facets.Records = append(table.Desired.Features.Facets.Records, indexFacetRecord(c, protocol.ValueInput_DESIRED, "by_label", declared))
	}
	if held != "" {
		table.Current.Features.Facets.Records = append(table.Current.Features.Facets.Records, indexFacetRecord(c, protocol.ValueInput_OBSERVED, "by_label", held))
	}
	return request
}

// TestDeclaredIndexSettings_HappyPath gives an index the table holds the
// partitioning and read replicas its own facet declares, naming every
// partitioning setting the index ends with when the partitioning changes, as
// the host's embedded engine writes a change of a held index's settings, and
// plans nothing for settings the index already holds: a setting the
// declaration leaves out keeps what the index holds.
func TestDeclaredIndexSettings_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, declared, held string
		statements           []string
	}{
		{"a partition count", `{"partitioningSettings":{"minPartitionsCount":"3"}}`, heldDefaults,
			[]string{"ALTER TABLE `/local/app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
				"AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);"}},
		{"read replicas", `{"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`, heldDefaults,
			[]string{"ALTER TABLE `/local/app/records` ALTER INDEX `by_label` SET (READ_REPLICAS_SETTINGS = \"PER_AZ:1\");"}},
		{"settings the index holds", `{"partitioningSettings":{"partitioningByLoad":"DISABLED"}}`, heldDefaults, nil},
		{"no settings declared", "", heldDefaults, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), indexSettingsRequest(c, test.declared, test.held))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(indexSettingsStatements(c, plan), qt.DeepEquals, test.statements)
		})
	}
}

// indexSettingsStatements are the statements of the plan's index settings
// steps, in step order.
func indexSettingsStatements(c *qt.C, plan *protocol.PlannedFeatures) []string {
	c.Helper()
	var statements []string
	for _, contribution := range plan.Contributions {
		for _, step := range contribution.Steps {
			if step.Operation.Value.Value.Kind != tablecreate.IndexSettingsOperationKind {
				continue
			}
			settings, err := tablecreate.DecodeIndexSettings(step.Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			statements = append(statements, tablecreate.IndexSettingsStatement("/local/app/records", settings))
		}
	}
	return statements
}

// TestDeclaredIndexSettings_FailurePath refuses a declared setting the plan
// cannot give: on an index the table neither holds nor gains, and on an
// index whose settings the read did not report.
func TestDeclaredIndexSettings_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	unheld := indexSettingsRequest(c, `{"partitioningSettings":{"minPartitionsCount":"3"}}`, heldDefaults)
	unheld.Tables[0].Current.Indexes, unheld.Tables[0].Desired.Indexes = nil, nil
	for _, test := range []struct {
		name       string
		request    *protocol.PlanFeaturesRequest
		wantReason string
	}{
		{"an index the table does not hold", unheld,
			`Index "by_label" declares its partitioning or read replicas, and the table neither holds it nor gains it`},
		{"settings the read did not report", indexSettingsRequest(c, `{"partitioningSettings":{"minPartitionsCount":"3"}}`, ""),
			`The settings index "by_label" holds were not read, so its declared settings cannot be compared`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), test.request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, test.wantReason)
		})
	}
}

// TestDeclaredIndexSettings_CreatedTable gives a table the plan creates the
// settings its indexes' own facets declare: one step per index after the
// creation, since YDB takes an index's settings only once the index exists.
// The step names every setting the new index ends with, where the host's
// embedded engine names the declared ones; YDB gives the index the same.
func TestDeclaredIndexSettings_CreatedTable(t *testing.T) {
	c := testcheck.New(t)
	request := indexSettingsRequest(c, `{"partitioningSettings":{"minPartitionsCount":"2","partitioningByLoad":"ENABLED"}}`, "")
	table := request.Tables[0]
	table.Action, table.Current = protocol.PlanningTable_CREATE, nil
	table.Desired.Features.Facets.Records = withoutKind(table.Desired.Features.Facets.Records, schema.TableProperties)
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(indexSettingsStatements(c, plan), qt.DeepEquals, []string{
		"ALTER TABLE `/local/app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
			"AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);"})
	steps := plan.Contributions[0].Steps
	c.Assert(steps, qt.HasLen, 2)
	c.Assert(steps[0].Id.Name, qt.Equals, "row-table-create/000000")
	c.Assert(steps[1].Id.Name, qt.Equals, "row-table-index-settings/000000")
	c.Assert(plan.Contributions[0].Dependencies, qt.HasLen, 1)
	c.Assert(proto.Equal(plan.Contributions[0].Dependencies[0], &protocol.PlanningDependency{Before: steps[0].Id, After: steps[1].Id}), qt.IsTrue)
	c.Assert(plan.Parents[0].Receipt.Steps, qt.HasLen, 2)
}

// withoutKind is records without those of kind.
func withoutKind(records []*protocol.ComparisonRecord, kind string) []*protocol.ComparisonRecord {
	var kept []*protocol.ComparisonRecord
	for _, record := range records {
		if record.Kind != kind {
			kept = append(kept, record)
		}
	}
	return kept
}

// indexSettingsChangeOf is the change of by_label's settings from what held
// reports into what declared states, as the host passes it to the planner.
func indexSettingsChangeOf(c *qt.C, held, declared string) *protocol.PlanningChange {
	c.Helper()
	before := indexFacetRecord(c, protocol.ValueInput_OBSERVED, "by_label", held)
	after := indexFacetRecord(c, protocol.ValueInput_DESIRED, "by_label", declared)
	change, err := schema.CaptureTablePropertiesChange("change", before.Value.Value.Owner, before.Value.Value, after.Value.Value)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanningChange{Subject: proto.CloneOf(before.Subject), Value: change}
}

// The host plans the reverse of a change of an index's settings with the
// declaration the forward change started from, which states only some of the
// settings, and with the index's own facet as the forward change leaves it,
// while the table's index read stays as it was read. The plan gives the index
// what the reverse change asks for, every setting it held, against the facet:
// planned from the declaration alone, the partitioning by load the forward
// change turned on would stay, and planned against the table's index read,
// nothing would change.
func TestDeclaredIndexSettings_PlansTheReverseOfAChange(t *testing.T) {
	c := testcheck.New(t)
	const (
		forward  = `{"partitioningSettings":{"minPartitionsCount":"3","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","partitioningBySize":"ENABLED"}}`
		restored = `{"partitioningSettings":{"minPartitionsCount":"2","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"},` +
			`"readReplicasSettings":{"perAzReadReplicasCount":"0"}}`
	)
	request := indexSettingsRequest(c, `{"partitioningSettings":{"minPartitionsCount":"2"}}`, forward)
	request.Tables[0].Current.Features.Facets.Records = append(request.Tables[0].Current.Features.Facets.Records, observedFacet(c, schema.IndexProperties, "",
		`{"commonIndexes":[{"name":"by_label","indexColumns":["label"],"globalIndex":{"settings":{"partitioningSettings":{"partitioningBySize":"ENABLED",`+
			`"partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"2"}}}}]}`))
	request.Changes = []*protocol.PlanningChange{indexSettingsChangeOf(c, forward, restored)}

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(indexSettingsStatements(c, plan), qt.DeepEquals, []string{
		"ALTER TABLE `/local/app/records` ALTER INDEX `by_label` SET (AUTO_PARTITIONING_BY_SIZE = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 2048, " +
			"AUTO_PARTITIONING_BY_LOAD = DISABLED, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);"})
	c.Assert(plan.Changes, qt.HasLen, 1)
}
