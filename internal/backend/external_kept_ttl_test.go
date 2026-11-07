package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// keptTTLStorage is a column table's storage as YDB 26.2.1.14 describes one
// whose TTL moves rows to the data source app/bucket after a day and deletes
// them after a week.
const keptTTLStorage = `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64","ttl":{"enabled":{"column":"ts","expireAfterSeconds":"604800",` +
	`"tiers":[{"applyAfterSeconds":"86400","externalStorage":"/local/app/bucket"},{"applyAfterSeconds":"604800","delete":{}}]}}}`

// keptTTLTable is the column table app/records a plan keeps, read with the
// TTL keptTTLStorage holds.
func keptTTLTable(c *qt.C) *protocol.PlanningTable {
	c.Helper()
	var model *protocol.ValueModel
	for _, candidate := range schema.ObservedModels() {
		if candidate.Kind == schema.ColumnStorageProperties {
			model = candidate
		}
	}
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}
	subject := &protocol.ComparisonIdentity{Kind: "table", Schema: testcomparison.Part("app"), Name: testcomparison.Part("records")}
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "storage",
		Representation: protocol.ValueInput_OBSERVED, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.ColumnStorageProperties, SchemaVersion: 1,
			Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: []byte(keptTTLStorage)}}}})
	c.Assert(err, qt.IsNil)
	columns := []*protocol.DeclaredColumn{{Name: "ts", Type: "Timestamp", Primary: true}, {Name: "id", Type: "Uint64", Primary: true}}
	return &protocol.PlanningTable{Subject: subject, Action: protocol.PlanningTable_CONTEXT,
		Desired: &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"ts", "id"}, Columns: columns, Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
			Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}},
		Current: &protocol.ObservedTable{Name: "records", Schema: "app", Columns: []*protocol.ObservedColumn{{Name: "ts"}, {Name: "id"}}, Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}},
			Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{},
				Records: []*protocol.ComparisonRecord{{Kind: schema.ColumnStorageProperties, Subject: subject, Value: values.Values[0]}}}}}}
}

// A data source moved while a column table the plan keeps moves rows to it
// is changed between a RESET (TTL) of the table and a SET (TTL) that writes
// the same TTL again, whether the target replaces the source in place with
// CREATE OR REPLACE or drops it and creates it again: YDB replaces no data
// source a TTL names. The source's strategy says so. Ported from the host's
// TestGenerateMigrationAST_ColumnTTLSourceReplacement.
func TestExternalSourceReplacementCarriesAKeptTTL_HappyPath(t *testing.T) {
	bucket, moved := external{name: "bucket", source: bucketAt("https://a/")}, external{name: "bucket", source: bucketAt("https://b/")}
	for _, test := range []struct {
		name         string
		capabilities map[string]bool
		source       string
		strategy     string
	}{
		{name: "replaced in place", capabilities: withReplace(), source: "external-data-source/000000/alter",
			strategy: "replace the data source in place with CREATE OR REPLACE; the TTL of each column table that moves rows to it is reset before and set again after"},
		{name: "dropped and created again", capabilities: withSources(), source: "external-data-source/000000/alter",
			strategy: "drop the data source and create it again, with the external tables over it; the TTL of each column table that moves rows to it is reset before and set again after"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			capabilities := test.capabilities
			capabilities["tiered_ttl"] = true
			request := externalRequest(c, []external{moved}, []external{bucket}, capabilities)
			compared, err := backend.New().CompareModels(c.Context(), request)
			c.Assert(err, qt.IsNil)
			planning := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: externalplan.ServiceID, TargetFeatures: backend.Description().Features, DatabasePath: "/local",
				Identifiers: request.Identifiers, Capabilities: capabilities, Tables: []*protocol.PlanningTable{keptTTLTable(c)}}
			for _, change := range compared.Changes {
				planning.Changes = append(planning.Changes, &protocol.PlanningChange{Subject: change.Subject, Value: change.Value})
			}

			plan, err := backend.New().PlanFeatures(c.Context(), planning)

			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(provider.ValidatePlannedFeatures(backend.Description(), planning, plan), qt.IsNil)
			c.Assert(stepEdges(plan), qt.DeepEquals, []string{
				"column-table-kept-ttl/000000/reset -> " + test.source,
				test.source + " -> column-table-kept-ttl/000000/set",
			})
			c.Assert(plan.Changes[0].Strategy, qt.Equals, test.strategy)
			c.Assert(plan.Changes[0].Steps, qt.HasLen, 3)
		})
	}
}

// A target without tiered TTLs refuses the replacement: it could not set the
// TTL again.
func TestExternalSourceReplacementCarriesAKeptTTL_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := externalRequest(c, []external{{name: "bucket", source: bucketAt("https://b/")}}, []external{{name: "bucket", source: bucketAt("https://a/")}}, withReplace())
	compared, err := backend.New().CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: externalplan.ServiceID, TargetFeatures: backend.Description().Features, DatabasePath: "/local",
		Identifiers: request.Identifiers, Capabilities: withReplace(), Tables: []*protocol.PlanningTable{keptTTLTable(c)},
		Changes: []*protocol.PlanningChange{{Subject: compared.Changes[0].Subject, Value: compared.Changes[0].Value}}}

	plan, err := backend.New().PlanFeatures(c.Context(), planning)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Contributions, qt.HasLen, 0)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, "replacing external data source app/bucket means setting the TTL of column table app/records again, "+
		"which requires target capability tiered_ttl, unavailable on this ydb target.*")
}
