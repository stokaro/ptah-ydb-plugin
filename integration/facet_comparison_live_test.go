//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

// facetFixture holds one live row table app.records, the provider that reads
// it and the session the provider holds. The comparison runs in the provider
// subprocess against what the provider read back, so a difference the
// comparison reports is one a host would hand to planning.
type facetFixture struct {
	client    *providerruntime.Client
	session   *protocol.Session
	owner     *protocol.Reference
	directory string
	database  string
	// columns are the declared columns of a plan; nil declares id, ts and payload.
	columns []*protocol.DeclaredColumn
	// steps are the host steps a plan carries.
	steps []*protocol.CommonPlanningStep
	// version is the server version the fixture connected to.
	version string
}

// newFacetFixture creates records (id, ts, payload) with the given trailing
// table clauses, such as a WITH TTL option or a column family.
func newFacetFixture(c *qt.C, clauses, options string) *facetFixture {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	c.Cleanup(cancel)
	absolute := path.Join(database, directory, "records")
	_, err := opened.DB.ExecContext(ctx, "CREATE TABLE `"+absolute+"` (id Uint64, ts Timestamp, payload Utf8, PRIMARY KEY (id)"+clauses+")"+options)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	var version string
	c.Assert(opened.DB.QueryRowContext(ctx, "SELECT Version()").Scan(&version), qt.IsNil)
	return &facetFixture{client: client, session: session, directory: directory, database: database, version: version,
		owner: &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "records"}}
}

// facetState states the records of one side with complete knowledge of both
// facet models in that representation.
func facetState(representation protocol.ValueInput_Representation, records []*protocol.ComparisonRecord) *protocol.ComparisonState {
	models := slices.Clone(schema.Models())
	models = slices.DeleteFunc(models, func(model *protocol.ValueModel) bool {
		return model.Representation != representation || !slices.Contains(tablecompare.Kinds(), model.Kind)
	})
	state := &protocol.ComparisonState{Representation: representation, Coverage: &protocol.ComparisonCoverage{}, Records: records}
	for _, model := range models {
		state.Coverage.Kinds = append(state.Coverage.Kinds, &protocol.ComparisonKindCoverage{Model: model,
			Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}}})
	}
	return state
}

// compare asks the provider to compare the declared table facet with the facet
// it reads from the live table.
func (f *facetFixture) compare(c *qt.C, declared string) *protocol.ComparedModels {
	c.Helper()
	return f.compareRecords(c, desiredFacet(c, f.client, f.owner, schema.TableProperties, declared))
}

// compareRecords compares the given declared facets with the live table.
func (f *facetFixture) compareRecords(c *qt.C, desired ...*protocol.ComparisonRecord) *protocol.ComparedModels {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
	defer cancel()
	current := observedTable(c, f.client, f.session, f.directory).Features.Facets.Records
	type sides struct {
		subject          *protocol.ComparisonIdentity
		desired, current bool
	}
	held := map[string]sides{}
	for _, record := range desired {
		key := record.Subject.Kind + "/" + record.Subject.Name.Normalized
		held[key] = sides{subject: record.Subject, desired: true, current: held[key].current}
	}
	for _, record := range current {
		key := record.Subject.Kind + "/" + record.Subject.Name.Normalized
		held[key] = sides{subject: record.Subject, desired: held[key].desired, current: true}
	}
	request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: tablecompare.ServiceID, Kinds: tablecompare.Kinds(),
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.session.Identifiers)},
		Desired:     facetState(protocol.ValueInput_DESIRED, desired), Current: facetState(protocol.ValueInput_OBSERVED, current)}
	for _, key := range slices.Sorted(maps.Keys(held)) {
		request.Parents = append(request.Parents, &protocol.ComparisonParent{Subject: held[key].subject, Desired: held[key].desired, Current: held[key].current})
	}
	result, err := f.client.CompareModels(ctx, request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Undecided, qt.HasLen, 0)
	return result
}

// apply plans the reported changes against the live table, renders the plan
// and executes it.
func (f *facetFixture) apply(c *qt.C, id, declared string, changes []*protocol.ComparedChange) {
	c.Helper()
	f.applyRecords(c, id, changes, desiredFacet(c, f.client, f.owner, schema.TableProperties, declared))
}

// plan plans the reported changes against the live table.
func (f *facetFixture) plan(c *qt.C, changes []*protocol.ComparedChange, records ...*protocol.ComparisonRecord) *protocol.PlannedFeatures {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	columns := f.columns
	if columns == nil {
		columns = []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true}, {Name: "payload", Type: "Utf8", Nullable: true}}
	}
	desired := &protocol.DeclaredTable{Name: "records", Schema: f.directory, PrimaryKey: []string{"id"}, Columns: columns,
		Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
			Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{},
				Records: records}}}
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: f.database,
		Capabilities: f.session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.session.Identifiers)}, CommonSteps: f.steps,
		Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(f.owner), Action: protocol.PlanningTable_ALTER, Desired: desired,
			Current: observedTable(c, f.client, f.session, f.directory)}}}
	for _, change := range changes {
		request.Changes = append(request.Changes, &protocol.PlanningChange{Subject: proto.CloneOf(change.Subject), Value: proto.CloneOf(change.Value)})
	}
	plan, err := f.client.PlanFeatures(ctx, request)
	c.Assert(err, qt.IsNil)
	return plan
}

// applyRecords plans the reported changes, renders the plan and executes it.
func (f *facetFixture) applyRecords(c *qt.C, id string, changes []*protocol.ComparedChange, records ...*protocol.ComparisonRecord) {
	c.Helper()
	plan := f.plan(c, changes, records...)
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	c.Assert(plan.Changes, qt.HasLen, len(changes))
	c.Assert(plan.Contributions, qt.HasLen, 1)
	execution := f.execute(c, id, plan)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
}

// execute renders the steps of a plan in the order its dependencies give and
// runs them.
func (f *facetFixture) execute(c *qt.C, id string, plan *protocol.PlannedFeatures) *protocol.Execution {
	c.Helper()
	execution, err := f.tryExecute(c, id, plan)
	c.Assert(err, qt.IsNil)
	return execution
}

// tryExecute is execute for a plan that may not apply: the client reports an
// outcome it cannot confirm as an error.
func (f *facetFixture) tryExecute(c *qt.C, id string, plan *protocol.PlannedFeatures) (*protocol.Execution, error) {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	rendered, err := f.client.Render(ctx, renderSteps(scheduled(c, plan), f.session.Capabilities))
	c.Assert(err, qt.IsNil)
	return f.client.Execute(ctx, &protocol.ExecuteRequest{SessionId: f.session.Id, RequestId: id, Units: rendered.Units})
}

// heldTable is the table facet payload the provider reads back.
func (f *facetFixture) heldTable(c *qt.C) []byte {
	c.Helper()
	records := observedTable(c, f.client, f.session, f.directory).Features.Facets.Records
	return records[slices.IndexFunc(records, func(record *protocol.ComparisonRecord) bool { return record.Kind == schema.TableProperties })].Value.Value.Payload
}

// heldTTL is the TTL the provider reads back, as JSON, or "null" for none.
func (f *facetFixture) heldTTL(c *qt.C) string {
	c.Helper()
	var properties struct {
		TTL any `json:"ttlSettings"`
	}
	c.Assert(json.Unmarshal(f.heldTable(c), &properties), qt.IsNil)
	encoded, err := json.Marshal(properties.TTL)
	c.Assert(err, qt.IsNil)
	return string(encoded)
}

const (
	hourlyTTL = `{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}`
	dailyTTL  = `{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":86400}}`
)

func ttlDeclaration(ttl string) string {
	return `{"primaryKey":["id"],"ttlSettings":` + ttl + `}`
}

const noTTLDeclaration = `{"primaryKey":["id"]}`

func TestFacetComparisonTTLSubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newFacetFixture(c, "", "")
	// Each step starts from what the previous one left in the table.
	for _, step := range []struct {
		name     string
		declared string
		want     string
	}{
		{"sets a TTL on a table without one", ttlDeclaration(hourlyTTL), hourlyTTL},
		{"changes the interval", ttlDeclaration(dailyTTL), dailyTTL},
		{"removes the TTL a declaration leaves out", noTTLDeclaration, "null"},
	} {
		t.Run(step.name, func(t *testing.T) {
			c := qt.New(t)
			found := fixture.compare(c, step.declared)
			c.Assert(found.Changes, qt.HasLen, 1)
			c.Assert(found.Changes[0].Kind, qt.Equals, schema.TableProperties)
			c.Assert(found.Changes[0].Value.Value.Kind, qt.Equals, schema.TablePropertiesChange)
			fixture.apply(c, "ttl-"+step.name, step.declared, found.Changes)
			c.Assert(fixture.heldTTL(c), qt.JSONEquals, json.RawMessage(step.want))
			// The read-back is what the next comparison is made against: nothing is left to do.
			c.Assert(fixture.compare(c, step.declared).Changes, qt.HasLen, 0)
		})
	}
}

func TestFacetComparisonColumnFamilySubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newFacetFixture(c, ", FAMILY archive (COMPRESSION = 'off')", "")
	declared := `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"}]}`
	found := fixture.compare(c, declared)
	c.Assert(found.Changes, qt.HasLen, 1)
	c.Assert(found.Changes[0].Kind, qt.Equals, schema.TableProperties)
	fixture.apply(c, "family-compression", declared, found.Changes)
	c.Assert(string(fixture.heldTable(c)), qt.Contains, "COMPRESSION_LZ4")
	c.Assert(fixture.compare(c, declared).Changes, qt.HasLen, 0)
}

func TestFacetComparisonIdenticalTableSubprocess(t *testing.T) {
	for _, test := range []struct {
		name     string
		options  string
		declared string
	}{
		{"without a TTL", "", noTTLDeclaration},
		{"with the same TTL", ` WITH (TTL = Interval("PT1H") ON ts)`, ttlDeclaration(hourlyTTL)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := newFacetFixture(c, "", test.options)
			c.Assert(fixture.compare(c, test.declared).Changes, qt.HasLen, 0)
		})
	}
}
