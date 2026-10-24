//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
)

// columnTableFixture holds a provider session on the monitored test address
// and a fixture directory a column table app.records is created in.
type columnTableFixture struct {
	client    *providerruntime.Client
	session   *protocol.Session
	owner     *protocol.Reference
	directory string
	database  string
}

func newColumnTableFixture(c *qt.C, address string) *columnTableFixture {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	absolute := path.Join(database, directory, "records")
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	return &columnTableFixture{client: client, session: session, directory: directory, database: database,
		owner: &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "records"}}
}

// declaration declares records as a column table keyed by (ts, id) with the
// given column storage.
func (f *columnTableFixture) declaration(c *qt.C, storage string) *protocol.DeclaredTable {
	c.Helper()
	return &protocol.DeclaredTable{Name: "records", Schema: f.directory, PrimaryKey: []string{"ts", "id"},
		Columns: []*protocol.DeclaredColumn{{Name: "ts", Type: "Timestamp", Primary: true}, {Name: "id", Type: "Uint64", Primary: true}, {Name: "body", Type: "Utf8", Nullable: true}},
		Features: &protocol.CapturedFeatures{
			Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
			Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: facetState(protocol.ValueInput_DESIRED, nil).Coverage,
				Records: []*protocol.ComparisonRecord{desiredFacet(c, f.client, f.owner, schema.ColumnStorageProperties, storage)}}}}
}

// create plans, renders and executes the creation of records with the given
// storage in session, and returns the outcome.
func (f *columnTableFixture) create(c *qt.C, session *protocol.Session, id, storage string) (*protocol.Execution, error) {
	c.Helper()
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	subject := testplanning.Subject(f.owner)
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: f.database,
		Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
		Tables: []*protocol.PlanningTable{{Subject: subject, Action: protocol.PlanningTable_CREATE, Desired: f.declaration(c, storage)}},
		CommonSteps: []*protocol.CommonPlanningStep{{Id: &protocol.StepIdentity{Owner: "host", Name: "create"}, Parent: proto.CloneOf(subject),
			Metadata: &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Impact: protocol.ChangeSemantics_ADDITIVE, Reason: "Creates a table",
				Effects: []*protocol.PlanningEffect{{Subject: proto.CloneOf(subject), Action: protocol.PlanningEffect_CREATE}}}}}}
	prepareCapturedCreation(c, f.client, planning)
	plan, err := f.client.PlanFeatures(ctx, planning)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps[0].Operation.Value.Value.Kind, qt.Equals, tablecreate.ColumnOperationKind)
	rendered, err := f.client.Render(ctx, testplanning.RenderRequest(plan, session.Capabilities))
	c.Assert(err, qt.IsNil)
	return f.client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: id, Units: rendered.Units})
}

// held reads records back and returns its column-storage observation.
func (f *columnTableFixture) held(c *qt.C) *native.ColumnStorageObservation {
	c.Helper()
	records := observedTable(c, f.client, f.session, f.directory).Features.Facets.Records
	var storage *native.ColumnStorageObservation
	for _, record := range records {
		c.Assert(record.Kind, qt.Not(qt.Equals), schema.TableProperties, qt.Commentf("a column table read as a row table"))
		if record.Kind == schema.ColumnStorageProperties {
			storage = &native.ColumnStorageObservation{}
			c.Assert(protojson.Unmarshal(record.Value.Value.Payload, storage), qt.IsNil)
		}
	}
	c.Assert(storage, qt.IsNotNil)
	return storage
}

// compare compares the declared column storage with what the provider reads
// back from records.
func (f *columnTableFixture) compare(c *qt.C, storage string) *protocol.ComparedModels {
	c.Helper()
	current := observedTable(c, f.client, f.session, f.directory).Features.Facets.Records
	subject := testplanning.Subject(f.owner)
	request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: tablecompare.ServiceID, Kinds: tablecompare.Kinds(),
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.session.Identifiers)},
		Desired:     facetState(protocol.ValueInput_DESIRED, []*protocol.ComparisonRecord{desiredFacet(c, f.client, f.owner, schema.ColumnStorageProperties, storage)}),
		Current:     facetState(protocol.ValueInput_OBSERVED, current), Parents: []*protocol.ComparisonParent{{Subject: subject, Desired: true, Current: true}}}
	result, err := f.client.CompareModels(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Undecided, qt.HasLen, 0)
	return result
}

// alter plans the changes a comparison reported against the live table.
func (f *columnTableFixture) alter(c *qt.C, storage string, changes []*protocol.ComparedChange) *protocol.PlannedFeatures {
	c.Helper()
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: f.database,
		Capabilities: f.session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(f.session.Identifiers)},
		Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(f.owner), Action: protocol.PlanningTable_ALTER, Desired: f.declaration(c, storage),
			Current: observedTable(c, f.client, f.session, f.directory)}}}
	for _, change := range changes {
		request.Changes = append(request.Changes, &protocol.PlanningChange{Subject: proto.CloneOf(change.Subject), Value: proto.CloneOf(change.Value)})
	}
	plan, err := f.client.PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	return plan
}

// change compares the declared column storage with records, plans the change
// the comparison reports, renders it and executes it in the fixture's session.
// It returns the plan and the outcome.
func (f *columnTableFixture) change(c *qt.C, id, storage string) (*protocol.PlannedFeatures, *protocol.Execution, error) {
	c.Helper()
	changes := f.compare(c, storage).Changes
	c.Assert(changes, qt.HasLen, 1)
	plan := f.alter(c, storage, changes)
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	c.Assert(plan.Contributions, qt.HasLen, 1)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	defer cancel()
	rendered, err := f.client.Render(ctx, testplanning.RenderRequest(plan, f.session.Capabilities))
	c.Assert(err, qt.IsNil)
	execution, err := f.client.Execute(ctx, &protocol.ExecuteRequest{SessionId: f.session.Id, RequestId: id, Units: rendered.Units})
	return plan, execution, err
}

// stepNames are the names of a plan's steps, in order.
func stepNames(plan *protocol.PlannedFeatures) []string {
	var names []string
	for _, step := range plan.GetContributions()[0].GetSteps() {
		names = append(names, step.Id.Name)
	}
	return names
}

const columnStorage = `{"hashColumns":["id"],"partitions":"3","ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`

// TestColumnTableCreationSubprocess creates a column table with a hash key, a
// shard count and a TTL through the native planning path, reads all three
// back, finds nothing to change on a second run, and refuses a change of the
// shard count with the reason.
func TestColumnTableCreationSubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newColumnTableFixture(c, monitoredAddress(c))
	execution, err := fixture.create(c, fixture.session, "create-column-table", columnStorage)
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	held := fixture.held(c)
	c.Assert(held.HashColumns, qt.DeepEquals, []string{"id"})
	c.Assert(held.Partitions, qt.Equals, uint64(3))
	c.Assert(held.GetTtl().GetEnabled().GetColumn(), qt.Equals, "ts")
	c.Assert(held.GetTtl().GetEnabled().GetTiers()[0].GetApplyAfterSeconds(), qt.Equals, uint64(86400))
	// A second run finds the table as declared.
	c.Assert(fixture.compare(c, columnStorage).Changes, qt.HasLen, 0)
	same := fixture.alter(c, columnStorage, nil)
	c.Assert(same.Diagnostics, qt.HasLen, 0)
	c.Assert(same.Contributions, qt.HasLen, 0)
	// A declaration that leaves the hash key and the shard count to the server
	// takes what the table holds.
	c.Assert(fixture.compare(c, `{"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`).Changes, qt.HasLen, 0)
	// The shard count is fixed when YDB creates the table.
	reshard := `{"hashColumns":["id"],"partitions":"4","ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`
	changes := fixture.compare(c, reshard).Changes
	c.Assert(changes, qt.HasLen, 1)
	c.Assert(changes[0].Value.Value.Kind, qt.Equals, schema.ColumnStorageChange)
	refused := fixture.alter(c, reshard, changes)
	c.Assert(refused.Diagnostics, qt.HasLen, 1)
	c.Assert(refused.Diagnostics[0].Message, qt.Matches, `table ".*/records": changing a column table's hash key or shard count requires an explicit data migration \(the table has 3 shards, and the declaration asks for 4\)`)
	c.Assert(refused.Contributions, qt.HasLen, 0)
	// An occupied path cannot turn a captured CREATE into a replacement.
	again, err := fixture.create(c, fixture.session, "stale-create", columnStorage)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(again.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
}

// TestColumnTableCreationWithoutMonitoringSubprocess refuses the creation
// before anything is written in a session whose address names no monitoring
// endpoint, since the creation is confirmed by the shard count read there.
func TestColumnTableCreationWithoutMonitoringSubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newColumnTableFixture(c, monitoredAddress(c))
	plain, err := fixture.client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: withoutMonitoring(c, monitoredAddress(c))})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(fixture.client.CloseSession(context.Background(), plain.Id), qt.IsNil) })
	execution, err := fixture.create(c, plain, "unmonitored-create", columnStorage)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	listing, err := fixture.client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: fixture.session.Id, Schemas: []string{fixture.directory}})
	c.Assert(err, qt.IsNil)
	c.Assert(listing.Tables, qt.HasLen, 0)
}

// TestColumnTableTieredTTLCreationSubprocess turns tiering on through the
// server's dynamic configuration, creates a column table whose TTL moves rows
// to an external data source before it deletes them, and reads the tiers back.
func TestColumnTableTieredTTLCreationSubprocess(t *testing.T) {
	c := qt.New(t)
	address := monitoredAddress(c)
	parsed, err := connection.Parse(address)
	c.Assert(err, qt.IsNil)
	opened, err := connection.Open(c.Context(), parsed)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(opened.Close(), qt.IsNil) })
	newClusterFlags(c, opened).set(c, map[string]bool{"EnableTieringInColumnShard": true, "EnableExternalDataSources": true})
	fixture := newColumnTableFixture(c, address)
	c.Assert(fixture.session.Capabilities["tiered_ttl"], qt.IsTrue)
	suffix := strings.ToLower(rand.Text())
	access, secret := "ptah_ct_access_"+suffix, "ptah_ct_secret_"+suffix
	source := path.Join(fixture.database, fixture.directory, "archive")
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	for _, statement := range []string{
		"CREATE OBJECT " + access + " (TYPE SECRET) WITH (value='fixture-access')",
		"CREATE OBJECT " + secret + " (TYPE SECRET) WITH (value='fixture-secret')",
		"CREATE EXTERNAL DATA SOURCE `" + source + "` WITH (SOURCE_TYPE='ObjectStorage', LOCATION='https://column.invalid/archive/', AUTH_METHOD='AWS', " +
			"AWS_REGION='us-east-1', AWS_ACCESS_KEY_ID_SECRET_NAME='" + access + "', AWS_SECRET_ACCESS_KEY_SECRET_NAME='" + secret + "')",
	} {
		_, err := opened.DB.ExecContext(ctx, statement)
		c.Assert(err, qt.IsNil, qt.Commentf("%s", statement))
	}
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		for _, statement := range []string{"DROP TABLE IF EXISTS `" + path.Join(fixture.database, fixture.directory, "records") + "`",
			"DROP EXTERNAL DATA SOURCE IF EXISTS `" + source + "`", "DROP OBJECT " + access + " (TYPE SECRET)", "DROP OBJECT " + secret + " (TYPE SECRET)"} {
			c.Check(execFixtureSchema(cleanup, opened.DB, statement), qt.IsNil, qt.Commentf("%s", statement))
		}
	})
	storage := `{"hashColumns":["id"],"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","externalStorage":"` + source + `"},` +
		`{"applyAfterSeconds":"604800","delete":{}}]}}}`
	execution, err := fixture.create(c, fixture.session, "create-tiered-column-table", storage)
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	tiers := fixture.held(c).GetTtl().GetEnabled().GetTiers()
	c.Assert(tiers, qt.HasLen, 2)
	c.Assert(tiers[0].GetExternalStorage(), qt.Equals, source)
	c.Assert(tiers[0].GetApplyAfterSeconds(), qt.Equals, uint64(86400))
	c.Assert(tiers[1].GetDelete(), qt.IsNotNil)
	c.Assert(fixture.compare(c, storage).Changes, qt.HasLen, 0)
	// The tiered TTL changes in place: the held policy lets go of its source,
	// and the declared one moves rows to it after two days.
	moved := `{"hashColumns":["id"],"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"172800","externalStorage":"` + source + `"},` +
		`{"applyAfterSeconds":"604800","delete":{}}]}}}`
	plan, execution, err := fixture.change(c, "move-tiered-ttl", moved)
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	c.Assert(stepNames(plan), qt.DeepEquals, []string{"column-table-ttl/000000/reset", "column-table-ttl/000000/set"})
	tiers = fixture.held(c).GetTtl().GetEnabled().GetTiers()
	c.Assert(tiers, qt.HasLen, 2)
	c.Assert(tiers[0].GetExternalStorage(), qt.Equals, source)
	c.Assert(tiers[0].GetApplyAfterSeconds(), qt.Equals, uint64(172800))
	c.Assert(fixture.compare(c, moved).Changes, qt.HasLen, 0)
}

// TestColumnTableTTLChangeSubprocess changes the TTL of a column table in place
// through the native planning path, as the host's embedded engine does: a
// RESET of the held TTL, then a SET of the declared one. It removes the TTL,
// sets one on the table without it, and refuses a step planned against a TTL
// the table no longer holds before anything is written.
func TestColumnTableTTLChangeSubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newColumnTableFixture(c, monitoredAddress(c))
	execution, err := fixture.create(c, fixture.session, "create-column-table", columnStorage)
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	const (
		twoDays = `{"hashColumns":["id"],"partitions":"3","ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"172800","delete":{}}]}}}`
		noTTL   = `{"hashColumns":["id"],"partitions":"3"}`
	)
	for _, test := range []struct {
		name, storage string
		steps         []string
		seconds       []uint64
	}{
		{"another interval", twoDays, []string{"column-table-ttl/000000/reset", "column-table-ttl/000000/set"}, []uint64{172800}},
		{"no TTL", noTTL, []string{"column-table-ttl/000000/reset"}, nil},
		{"a TTL on a table without one", columnStorage, []string{"column-table-ttl/000000/set"}, []uint64{86400}},
	} {
		plan, execution, err := fixture.change(c, "ttl-"+test.name, test.storage)
		c.Assert(err, qt.IsNil, qt.Commentf("%s", test.name))
		c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s: %s", test.name, execution))
		c.Assert(stepNames(plan), qt.DeepEquals, test.steps, qt.Commentf("%s", test.name))
		var seconds []uint64
		for _, tier := range fixture.held(c).GetTtl().GetEnabled().GetTiers() {
			seconds = append(seconds, tier.GetApplyAfterSeconds())
		}
		c.Assert(seconds, qt.DeepEquals, test.seconds, qt.Commentf("%s", test.name))
		c.Assert(fixture.compare(c, test.storage).Changes, qt.HasLen, 0, qt.Commentf("%s", test.name))
	}
	// A step planned against the daily TTL, run after another plan changed it.
	changes := fixture.compare(c, twoDays).Changes
	stale := fixture.alter(c, twoDays, changes)
	_, execution, err = fixture.change(c, "ttl-moved-on", noTTL)
	c.Assert(err, qt.IsNil)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
	rendered, err := fixture.client.Render(c.Context(), testplanning.RenderRequest(stale, fixture.session.Capabilities))
	c.Assert(err, qt.IsNil)
	execution, err = fixture.client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: fixture.session.Id, RequestId: "ttl-stale", Units: rendered.Units})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(fixture.held(c).GetTtl().GetEnabled(), qt.IsNil)
}
