//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path"
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
		result.Columns = append(result.Columns, &protocol.ObservedColumn{Name: column.Name})
	}
	for _, value := range normalized.Values {
		owner := value.Value.Owner
		if value.Value.Kind != schema.TableProperties && value.Value.Kind != schema.ColumnProperties {
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
	if kind == schema.ColumnProperties {
		model = schema.ColumnDesiredModel()
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
		request := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
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
