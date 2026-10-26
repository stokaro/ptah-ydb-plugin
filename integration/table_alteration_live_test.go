//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"slices"
	"strconv"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

// observedTable reads app.records back through the provider and captures it as
// a host would hand it to planning: columns plus the observed table and column
// facets, each normalized by the provider.
func observedTable(c *qt.C, client *providerruntime.Client, session *protocol.Session, directory string) *protocol.ObservedTable {
	c.Helper()
	request := &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}}
	snapshot, err := client.Inspect(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.Tables, qt.HasLen, 1)
	values, err := provider.InspectionValues(client.Description(), request, snapshot)
	c.Assert(err, qt.IsNil)
	normalized, err := client.NormalizeValues(c.Context(), values)
	c.Assert(err, qt.IsNil)
	result := &protocol.ObservedTable{Name: "records", Schema: directory, Features: &protocol.CapturedFeatures{
		Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}},
		Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}}}}
	for _, column := range snapshot.Tables[0].Columns {
		result.Columns = append(result.Columns, proto.CloneOf(column.Observed))
	}
	for _, value := range normalized.Values {
		owner := value.Value.Owner
		if value.Value.Kind != schema.TableProperties && value.Value.Kind != schema.ColumnProperties && value.Value.Kind != schema.IndexProperties &&
			value.Value.Kind != schema.ColumnStorageProperties {
			continue
		}
		subject := testplanning.Subject(owner)
		if owner.Kind == "column" {
			subject.Parent = &protocol.ComparisonPart{Source: "records", Normalized: "records"}
		}
		result.Features.Facets.Records = append(result.Features.Facets.Records, &protocol.ComparisonRecord{Kind: value.Value.Kind, Subject: subject, Value: value})
	}
	return result
}

func desiredFacet(c *qt.C, client *providerruntime.Client, owner *protocol.Reference, kind, payload string) *protocol.ComparisonRecord {
	c.Helper()
	model := schema.TableDesiredModel()
	switch kind {
	case schema.ColumnProperties:
		model = schema.ColumnDesiredModel()
	case schema.IndexProperties, schema.ColumnStorageProperties:
		model = schema.NativeDesiredModel(kind)
	}
	values, err := client.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: kind + "/" + owner.Name,
		Representation: protocol.ValueInput_DESIRED, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: kind, SchemaVersion: 1,
			Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: owner, Payload: []byte(payload)}}}})
	c.Assert(err, qt.IsNil)
	subject := testplanning.Subject(owner)
	if owner.Kind == "column" {
		subject.Parent = &protocol.ComparisonPart{Source: "records", Normalized: "records"}
	}
	return &protocol.ComparisonRecord{Kind: kind, Subject: subject, Value: values.Values[0]}
}

// familyCase states the families and placements one alteration should reach.
type familyCase struct {
	name         string
	table        string
	columns      map[string]string
	wantAdd      int
	wantAlter    int
	wantMoves    int
	wantFamilies map[string]string
}

func TestRowTableFamilyAlterationSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	absolute := path.Join(database, directory, "records")
	_, err := opened.DB.ExecContext(ctx, "CREATE TABLE `"+absolute+"` (id Uint64, payload Utf8, extra Utf8, keep Utf8, PRIMARY KEY (id), FAMILY archive (COMPRESSION = 'off'))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	_, err = opened.DB.ExecContext(ctx, "UPSERT INTO `"+absolute+"` (id, payload, extra, keep) VALUES (1u, \"a\"u, \"b\"u, \"c\"u)")
	c.Assert(err, qt.IsNil)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "records"}
	columnOwner := func(name string) *protocol.Reference {
		return &protocol.Reference{Engine: "ydb", Kind: "column", Database: database, Schema: directory, Path: []string{"records"}, Name: name}
	}
	plan := func(c *qt.C, in familyCase) *protocol.PlannedFeatures {
		c.Helper()
		desired := &protocol.DeclaredTable{Name: "records", Schema: directory, PrimaryKey: []string{"id"},
			Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "payload", Type: "Utf8", Nullable: true},
				{Name: "extra", Type: "Utf8", Nullable: true}, {Name: "keep", Type: "Utf8", Nullable: true}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
				Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}}
		desired.Features.Facets.Records = append(desired.Features.Facets.Records, desiredFacet(c, client, owner, schema.TableProperties, in.table))
		for _, name := range []string{"payload", "extra", "keep"} {
			if payload, found := in.columns[name]; found {
				desired.Features.Facets.Records = append(desired.Features.Facets.Records, desiredFacet(c, client, columnOwner(name), schema.ColumnProperties, payload))
			}
		}
		request := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
			Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
			Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(owner), Action: protocol.PlanningTable_ALTER, Desired: desired,
				Current: observedTable(c, client, session, directory)}}}
		result, err := client.PlanFeatures(ctx, request)
		c.Assert(err, qt.IsNil)
		c.Assert(result.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", result))
		return result
	}
	var applied *protocol.RenderResult
	for _, step := range []familyCase{
		{name: "add families, change a setting and move columns",
			table:   `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4"},{"name":"cold"}]}`,
			columns: map[string]string{"payload": `{"family":"archive"}`, "extra": `{"family":"cold"}`},
			wantAdd: 1, wantAlter: 1, wantMoves: 2, wantFamilies: map[string]string{"payload": "archive", "extra": "cold", "keep": ""}},
		{name: "move a column back and drop a compression",
			table:     `{"primaryKey":["id"],"columnFamilies":[{"name":"archive","compression":"COMPRESSION_NONE"}]}`,
			columns:   map[string]string{"payload": `{}`, "extra": `{"family":"cold"}`},
			wantAlter: 1, wantMoves: 1, wantFamilies: map[string]string{"payload": "", "extra": "cold", "keep": ""}},
	} {
		t.Run(step.name, func(t *testing.T) {
			c := qt.New(t)
			result := plan(c, step)
			c.Assert(result.Contributions, qt.HasLen, 1)
			c.Assert(result.Contributions[0].Steps, qt.HasLen, 1)
			alteration, err := tablecreate.DecodeAlteration(result.Contributions[0].Steps[0].Operation.Value.Value)
			c.Assert(err, qt.IsNil)
			c.Assert(alteration.Request.AddColumnFamilies, qt.HasLen, step.wantAdd)
			c.Assert(alteration.Request.AlterColumnFamilies, qt.HasLen, step.wantAlter)
			c.Assert(alteration.Request.AlterColumns, qt.HasLen, step.wantMoves)
			applied, err = client.Render(ctx, testplanning.RenderRequest(result, session.Capabilities))
			c.Assert(err, qt.IsNil)
			c.Assert(applied.Units, qt.HasLen, 1)
			execution, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "alter-" + step.name, Units: applied.Units})
			c.Assert(err, qt.IsNil)
			c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
			// The read-back is what the next plan is made against: the same declaration changes nothing.
			again := plan(c, step)
			c.Assert(again.Contributions, qt.HasLen, 0)
			held := observedTable(c, client, session, directory)
			placed := map[string]string{}
			for _, record := range held.Features.Facets.Records {
				var properties struct {
					Family string `json:"family"`
				}
				c.Assert(json.Unmarshal(record.Value.Value.Payload, &properties), qt.IsNil)
				placed[record.Subject.Name.Source] = properties.Family
			}
			for name, family := range step.wantFamilies {
				c.Assert(placed[name], qt.Equals, family, qt.Commentf("column %s", name))
			}
			var count uint64
			c.Assert(opened.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+absolute+"` WHERE payload = \"a\"u AND extra = \"b\"u AND keep = \"c\"u").Scan(&count), qt.IsNil)
			c.Assert(count, qt.Equals, uint64(1))
		})
	}
	// A captured alteration whose table moved on is refused before dispatch.
	execution, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "stale-alter", Units: applied.Units})
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
}

// scheduled orders the provider's steps the way the host's scheduler would:
// each step after the steps the plan's dependencies put before it, ties kept in
// the order the provider listed them.
func scheduled(c *qt.C, plan *protocol.PlannedFeatures) []*protocol.PlanningStep {
	c.Helper()
	var steps []*protocol.PlanningStep
	var dependencies []*protocol.PlanningDependency
	for _, contribution := range plan.Contributions {
		steps = append(steps, contribution.Steps...)
		dependencies = append(dependencies, contribution.Dependencies...)
	}
	var ordered []*protocol.PlanningStep
	done := map[string]bool{}
	for len(ordered) < len(steps) {
		progressed := false
		for _, step := range steps {
			if done[step.Id.Name] {
				continue
			}
			ready := true
			for _, dependency := range dependencies {
				if dependency.After.Name == step.Id.Name && !done[dependency.Before.Name] && slices.ContainsFunc(steps, func(other *protocol.PlanningStep) bool { return other.Id.Name == dependency.Before.Name }) {
					ready = false
				}
			}
			if ready {
				done[step.Id.Name] = true
				ordered = append(ordered, step)
				progressed = true
				break
			}
		}
		c.Assert(progressed, qt.IsTrue, qt.Commentf("the plan has a cycle"))
	}
	return ordered
}

func renderSteps(steps []*protocol.PlanningStep, capabilities map[string]bool) *protocol.RenderRequest {
	request := &protocol.RenderRequest{Engine: "ydb", Capabilities: capabilities}
	for _, step := range steps {
		value := proto.CloneOf(step.Operation.Value)
		request.Changes = append(request.Changes, &protocol.Change{Id: value.Id, Object: proto.CloneOf(value.Value.Owner),
			Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Role: step.Operation.Role, Value: value}}})
	}
	return request
}

func TestRowTableColumnAlterationSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	absolute := path.Join(database, directory, "records")
	_, err := opened.DB.ExecContext(ctx, "CREATE TABLE `"+absolute+"` (id Uint64, keep Utf8, old Utf8, stale Utf8, marker Utf8, PRIMARY KEY (id), INDEX by_marker GLOBAL ON (marker), FAMILY archive (COMPRESSION = 'off'))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	_, err = opened.DB.ExecContext(ctx, "UPSERT INTO `"+absolute+"` (id, keep, old, stale, marker) VALUES (1u, \"k\"u, \"o\"u, \"s\"u, \"t\"u)")
	c.Assert(err, qt.IsNil)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "records"}
	columnOwner := func(name string) *protocol.Reference {
		return &protocol.Reference{Engine: "ydb", Kind: "column", Database: database, Schema: directory, Path: []string{"records"}, Name: name}
	}
	columnSubject := func(name string) *protocol.ComparisonIdentity {
		subject := testplanning.Subject(columnOwner(name))
		subject.Parent = &protocol.ComparisonPart{Source: "records", Normalized: "records"}
		return subject
	}
	table := testplanning.Subject(owner)
	addition := func(c *qt.C, id, name string, placed bool) *protocol.CommonPlanningStep {
		column := &protocol.PlanningColumn{Name: name, Type: "Utf8", Nullable: true}
		if placed {
			record := desiredFacet(c, client, columnOwner(name), schema.ColumnProperties, `{"family":"archive"}`)
			column.Facets = []*protocol.CapturedFacet{{Kind: record.Kind, Value: record.Value}}
		}
		return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: id}, Parent: proto.CloneOf(table), AddedColumn: column,
			Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation", Effects: []*protocol.PlanningEffect{
				{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ}, {Subject: columnSubject(name), Action: protocol.PlanningEffect_CREATE}}}}
	}
	removal := func(id, name string) *protocol.CommonPlanningStep {
		return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: id}, Parent: proto.CloneOf(table),
			Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation", Effects: []*protocol.PlanningEffect{
				{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ}, {Subject: columnSubject(name), Action: protocol.PlanningEffect_DROP}}}}
	}
	plan := func(c *qt.C, declared []string, familyTable string, columns map[string]string, steps ...*protocol.CommonPlanningStep) *protocol.PlannedFeatures {
		c.Helper()
		desired := &protocol.DeclaredTable{Name: "records", Schema: directory, PrimaryKey: []string{"id"},
			Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
				Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}}
		for _, name := range declared {
			desired.Columns = append(desired.Columns, &protocol.DeclaredColumn{Name: name, Type: "Utf8", Nullable: true})
		}
		desired.Features.Facets.Records = append(desired.Features.Facets.Records, desiredFacet(c, client, owner, schema.TableProperties, familyTable))
		for _, name := range declared {
			if payload, found := columns[name]; found {
				desired.Features.Facets.Records = append(desired.Features.Facets.Records, desiredFacet(c, client, columnOwner(name), schema.ColumnProperties, payload))
			}
		}
		request := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
			Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)}, CommonSteps: steps,
			Tables: []*protocol.PlanningTable{{Subject: proto.CloneOf(table), Action: protocol.PlanningTable_ALTER, Desired: desired, Current: observedTable(c, client, session, directory)}}}
		result, err := client.PlanFeatures(ctx, request)
		c.Assert(err, qt.IsNil)
		c.Assert(result.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", result))
		return result
	}
	columnNames := func(c *qt.C) []string {
		c.Helper()
		var names []string
		for _, column := range observedTable(c, client, session, directory).Columns {
			names = append(names, column.Name)
		}
		slices.Sort(names)
		return names
	}
	family := `{"primaryKey":["id"],"columnFamilies":[{"name":"archive"}]}`
	var applied *protocol.RenderResult
	t.Run("adds a column, places it in a family, and drops columns", func(t *testing.T) {
		c := qt.New(t)
		// The host lists the removals first; the family step still follows the
		// addition it moves a column into and precedes every removal.
		result := plan(c, []string{"keep", "marker", "fresh", "plain"}, family, map[string]string{"fresh": `{"family":"archive"}`},
			removal("drop-old", "old"), removal("drop-stale", "stale"), addition(c, "add-fresh", "fresh", true), addition(c, "add-plain", "plain", false))
		c.Assert(result.Rewrites, qt.HasLen, 4)
		c.Assert(result.Contributions[0].Steps, qt.HasLen, 5)
		ordered := scheduled(c, result)
		var names []string
		for _, step := range ordered {
			names = append(names, step.Id.Name)
		}
		c.Assert(slices.Index(names, "row-table-add-column/000000") < slices.Index(names, "row-table-alter/000000"), qt.IsTrue, qt.Commentf("%v", names))
		c.Assert(slices.Index(names, "row-table-alter/000000") < slices.Index(names, "row-table-drop-column/000000"), qt.IsTrue, qt.Commentf("%v", names))
		c.Assert(slices.Index(names, "row-table-alter/000000") < slices.Index(names, "row-table-drop-column/000001"), qt.IsTrue, qt.Commentf("%v", names))
		var err error
		applied, err = client.Render(ctx, renderSteps(ordered, session.Capabilities))
		c.Assert(err, qt.IsNil)
		c.Assert(applied.Units, qt.HasLen, 5)
		execution, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "alter-columns", Units: applied.Units})
		c.Assert(err, qt.IsNil)
		c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
		c.Assert(columnNames(c), qt.DeepEquals, []string{"fresh", "id", "keep", "marker", "plain"})
		// The read-back is what the next plan is made against: nothing is left to do.
		again := plan(c, []string{"keep", "marker", "fresh", "plain"}, family, map[string]string{"fresh": `{"family":"archive"}`})
		c.Assert(again.Contributions, qt.HasLen, 0)
		var placement string
		for _, record := range observedTable(c, client, session, directory).Features.Facets.Records {
			if record.Subject.Name.Source == "fresh" {
				var properties struct {
					Family string `json:"family"`
				}
				c.Assert(json.Unmarshal(record.Value.Value.Payload, &properties), qt.IsNil)
				placement = properties.Family
			}
		}
		c.Assert(placement, qt.Equals, "archive")
		var count uint64
		c.Assert(opened.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+absolute+"` WHERE keep = \"k\"u AND marker = \"t\"u AND fresh IS NULL AND plain IS NULL").Scan(&count), qt.IsNil)
		c.Assert(count, qt.Equals, uint64(1))
	})
	t.Run("refuses a column that an index uses before the call", func(t *testing.T) {
		c := qt.New(t)
		result := plan(c, []string{"keep", "fresh", "plain"}, family, map[string]string{"fresh": `{"family":"archive"}`}, removal("drop-marker", "marker"))
		c.Assert(result.Rewrites, qt.HasLen, 1)
		rendered, err := client.Render(ctx, renderSteps(scheduled(c, result), session.Capabilities))
		c.Assert(err, qt.IsNil)
		execution, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "drop-indexed", Units: rendered.Units})
		c.Assert(err, qt.ErrorIs, provider.ErrExecution)
		c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
		c.Assert(columnNames(c), qt.DeepEquals, []string{"fresh", "id", "keep", "marker", "plain"})
	})
	t.Run("refuses a stale alteration before the call", func(t *testing.T) {
		c := qt.New(t)
		execution, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "stale-columns", Units: applied.Units})
		c.Assert(err, qt.ErrorIs, provider.ErrExecution)
		c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	})
}

func TestRowTableTTLAlterationSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	absolute := path.Join(database, directory, "records")
	_, err := opened.DB.ExecContext(ctx, "CREATE TABLE `"+absolute+"` (id Uint64, ts Timestamp, stamp Uint64, keep Utf8, label Utf8, PRIMARY KEY (id))")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+absolute+"`"), qt.IsNil)
	})
	_, err = opened.DB.ExecContext(ctx, "UPSERT INTO `"+absolute+"` (id, keep) VALUES (1u, \"kept\"u)")
	c.Assert(err, qt.IsNil)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	session, err := client.Open(ctx, &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "records"}
	table := testplanning.Subject(owner)
	columnSubject := func(name string) *protocol.ComparisonIdentity {
		subject := testplanning.Subject(&protocol.Reference{Engine: "ydb", Kind: "column", Database: database, Schema: directory, Path: []string{"records"}, Name: name})
		subject.Parent = &protocol.ComparisonPart{Source: "records", Normalized: "records"}
		return subject
	}
	addition := func(id, name string) *protocol.CommonPlanningStep {
		return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: id}, Parent: proto.CloneOf(table),
			AddedColumn: &protocol.PlanningColumn{Name: name, Type: "Timestamp", Nullable: true},
			Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation", Effects: []*protocol.PlanningEffect{
				{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ}, {Subject: columnSubject(name), Action: protocol.PlanningEffect_CREATE}}}}
	}
	removal := func(id, name string) *protocol.CommonPlanningStep {
		return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "host", Name: id}, Parent: proto.CloneOf(table),
			Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Reason: "Fixture common operation", Effects: []*protocol.PlanningEffect{
				{Subject: proto.CloneOf(table), Action: protocol.PlanningEffect_READ}, {Subject: columnSubject(name), Action: protocol.PlanningEffect_DROP}}}}
	}
	// types are the declared columns beside the key; the table states its TTL as ttl, or none.
	plan := func(c *qt.C, types map[string]string, ttl string, steps ...*protocol.CommonPlanningStep) *protocol.PlannedFeatures {
		c.Helper()
		desired := &protocol.DeclaredTable{Name: "records", Schema: directory, PrimaryKey: []string{"id"},
			Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
				Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}}
		for _, name := range slices.Sorted(func(yield func(string) bool) {
			for name := range types {
				if !yield(name) {
					return
				}
			}
		}) {
			desired.Columns = append(desired.Columns, &protocol.DeclaredColumn{Name: name, Type: types[name], Nullable: true})
		}
		settings := `{"primaryKey":["id"]`
		if ttl != "" {
			settings += `,"ttlSettings":` + ttl
		}
		desired.Features.Facets.Records = append(desired.Features.Facets.Records, desiredFacet(c, client, owner, schema.TableProperties, settings+`}`))
		request := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
			Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)}, CommonSteps: steps,
			Tables: []*protocol.PlanningTable{{Subject: proto.CloneOf(table), Action: protocol.PlanningTable_ALTER, Desired: desired, Current: observedTable(c, client, session, directory)}}}
		result, err := client.PlanFeatures(ctx, request)
		c.Assert(err, qt.IsNil)
		c.Assert(result.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", result))
		return result
	}
	run := func(c *qt.C, id string, result *protocol.PlannedFeatures) ([]string, *protocol.RenderResult) {
		c.Helper()
		ordered := scheduled(c, result)
		var names []string
		for _, step := range ordered {
			names = append(names, step.Id.Name)
		}
		rendered, err := client.Render(ctx, renderSteps(ordered, session.Capabilities))
		c.Assert(err, qt.IsNil)
		execution, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: id, Units: rendered.Units})
		c.Assert(err, qt.IsNil)
		c.Assert(execution.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", execution))
		return names, rendered
	}
	// heldTTL is the TTL the provider reads back, as the next plan sees it.
	heldTTL := func(c *qt.C) string {
		c.Helper()
		for _, record := range observedTable(c, client, session, directory).Features.Facets.Records {
			if record.Kind != schema.TableProperties {
				continue
			}
			var properties struct {
				TTL any `json:"ttlSettings"`
			}
			c.Assert(json.Unmarshal(record.Value.Value.Payload, &properties), qt.IsNil)
			encoded, err := json.Marshal(properties.TTL)
			c.Assert(err, qt.IsNil)
			return string(encoded)
		}
		c.Fatal("the table facet was not read back")
		return ""
	}
	columnNames := func(c *qt.C) []string {
		c.Helper()
		var names []string
		for _, column := range observedTable(c, client, session, directory).Columns {
			names = append(names, column.Name)
		}
		slices.Sort(names)
		return names
	}
	date := func(column string, seconds int) string {
		return `{"dateTypeColumn":{"columnName":"` + column + `","expireAfterSeconds":` + strconv.Itoa(seconds) + `}}`
	}
	declared := map[string]string{"ts": "Timestamp", "stamp": "Uint64", "keep": "Utf8", "label": "Utf8"}
	var applied *protocol.RenderResult
	for _, step := range []struct {
		name string
		ttl  string
		want string
	}{
		{"sets a TTL on a table without one", date("ts", 3600), date("ts", 3600)},
		{"changes the interval", date("ts", 7200), date("ts", 7200)},
		{"reads an integer column", `{"valueSinceUnixEpoch":{"columnName":"stamp","columnUnit":"UNIT_MILLISECONDS","expireAfterSeconds":60}}`,
			`{"valueSinceUnixEpoch":{"columnName":"stamp","columnUnit":"UNIT_MILLISECONDS","expireAfterSeconds":60}}`},
		{"states a run interval", `{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`,
			`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600},"runIntervalSeconds":900}`},
		{"keeps the run interval the declaration leaves out", date("ts", 7200),
			`{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":7200},"runIntervalSeconds":900}`},
	} {
		t.Run(step.name, func(t *testing.T) {
			c := qt.New(t)
			result := plan(c, declared, step.ttl)
			c.Assert(result.Contributions, qt.HasLen, 1)
			c.Assert(result.Contributions[0].Steps, qt.HasLen, 1)
			c.Assert(result.Contributions[0].Steps[0].Id.Name, qt.Equals, "row-table-ttl/000000")
			_, applied = run(c, "ttl-"+step.name, result)
			c.Assert(heldTTL(c), qt.JSONEquals, json.RawMessage(step.want))
			// The read-back is what the next plan is made against: the same declaration changes nothing.
			c.Assert(plan(c, declared, step.ttl).Contributions, qt.HasLen, 0)
		})
	}
	t.Run("refuses a stale alteration before the call", func(t *testing.T) {
		c := qt.New(t)
		// The table now holds a TTL the captured alteration did not expect.
		execution, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "stale-ttl", Units: applied.Units})
		c.Assert(err, qt.ErrorIs, provider.ErrExecution)
		c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	})
	t.Run("moves the TTL to an added column and drops the column it read", func(t *testing.T) {
		c := qt.New(t)
		// The host lists the removal first. The server refuses a TTL on a column that
		// does not exist yet, and refuses to drop the column a TTL reads, so only the
		// order add, set TTL, drop can succeed.
		moved := map[string]string{"fresh": "Timestamp", "stamp": "Uint64", "keep": "Utf8", "label": "Utf8"}
		result := plan(c, moved, date("fresh", 3600), removal("drop-ts", "ts"), addition("add-fresh", "fresh"))
		c.Assert(result.Rewrites, qt.HasLen, 2)
		names, _ := run(c, "ttl-move", result)
		c.Assert(slices.Index(names, "row-table-add-column/000000") < slices.Index(names, "row-table-ttl/000000"), qt.IsTrue, qt.Commentf("%v", names))
		c.Assert(slices.Index(names, "row-table-ttl/000000") < slices.Index(names, "row-table-drop-column/000000"), qt.IsTrue, qt.Commentf("%v", names))
		c.Assert(columnNames(c), qt.DeepEquals, []string{"fresh", "id", "keep", "label", "stamp"})
		// The run interval the table held survives the move, since the declaration states none.
		c.Assert(heldTTL(c), qt.JSONEquals, json.RawMessage(`{"dateTypeColumn":{"columnName":"fresh","expireAfterSeconds":3600},"runIntervalSeconds":900}`))
		c.Assert(plan(c, moved, date("fresh", 3600)).Contributions, qt.HasLen, 0)
		var count uint64
		c.Assert(opened.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+absolute+"` WHERE keep = \"kept\"u").Scan(&count), qt.IsNil)
		c.Assert(count, qt.Equals, uint64(1))
	})
	t.Run("refuses a column the server would refuse before the call", func(t *testing.T) {
		c := qt.New(t)
		request := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
			Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)}}
		desired := &protocol.DeclaredTable{Name: "records", Schema: directory, PrimaryKey: []string{"id"},
			Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "label", Type: "Utf8", Nullable: true}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
				Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{},
					Records: []*protocol.ComparisonRecord{desiredFacet(c, client, owner, schema.TableProperties, `{"primaryKey":["id"],"ttlSettings":`+date("label", 60)+`}`)}}}}
		request.Tables = []*protocol.PlanningTable{{Subject: proto.CloneOf(table), Action: protocol.PlanningTable_ALTER, Desired: desired, Current: observedTable(c, client, session, directory)}}
		result, err := client.PlanFeatures(ctx, request)
		c.Assert(err, qt.IsNil)
		c.Assert(result.Diagnostics, qt.HasLen, 1)
		c.Assert(result.Diagnostics[0].Message, qt.Matches, `the TTL column "label" is Utf8, and YDB reads a TTL from.*`)
		c.Assert(result.Contributions, qt.HasLen, 0)
	})
	t.Run("removes the TTL a declaration leaves out", func(t *testing.T) {
		c := qt.New(t)
		moved := map[string]string{"fresh": "Timestamp", "stamp": "Uint64", "keep": "Utf8", "label": "Utf8"}
		result := plan(c, moved, "")
		c.Assert(result.Contributions, qt.HasLen, 1)
		run(c, "ttl-drop", result)
		c.Assert(heldTTL(c), qt.Equals, "null")
		c.Assert(plan(c, moved, "").Contributions, qt.HasLen, 0)
	})
}
