//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// layoutFixture creates app.records with the given key columns and starting
// layout fields through the provider, and returns the facet fixture that reads
// and alters it afterward.
func layoutFixture(c *qt.C, columns []*protocol.DeclaredColumn, key []string, fields string) *facetFixture {
	c.Helper()
	opened, directory, database := inspectionFixture(c)
	ctx, cancel := context.WithTimeout(c.Context(), 4*time.Minute)
	c.Cleanup(cancel)
	absolute := path.Join(database, directory, "records")
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
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: "records"}
	encodedKey, err := json.Marshal(key)
	c.Assert(err, qt.IsNil)
	source := &protocol.DeclaredTable{Name: owner.Name, Schema: owner.Schema, PrimaryKey: key, Columns: columns, Features: &protocol.CapturedFeatures{
		Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
		Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{},
			Records: []*protocol.ComparisonRecord{desiredFacet(c, client, owner, schema.TableProperties, `{"primaryKey":`+string(encodedKey)+`,`+fields+`}`)}}}}
	planning := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
		Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
		Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(owner), Action: protocol.PlanningTable_CREATE, Desired: source}}}
	prepareCapturedCreation(c, client, planning)
	projectCapturedCreation(c, client, planning)
	plan, err := client.PlanFeatures(ctx, planning)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	rendered, err := client.Render(ctx, testplanning.RenderRequest(plan, session.Capabilities))
	c.Assert(err, qt.IsNil)
	result, err := client.Execute(ctx, &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "create-with-layout", Units: rendered.Units})
	c.Assert(err, qt.IsNil)
	c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
	var version string
	c.Assert(opened.DB.QueryRowContext(ctx, "SELECT Version()").Scan(&version), qt.IsNil)
	return &facetFixture{client: client, session: session, owner: owner, directory: directory, database: database, columns: columns, version: version}
}

// heldMinimum is the minimum partition count the live table reports.
func (f *facetFixture) heldMinimum(c *qt.C) string {
	c.Helper()
	var properties struct {
		Partitioning struct {
			Minimum string `json:"minPartitionsCount"`
		} `json:"partitioningSettings"`
	}
	c.Assert(json.Unmarshal(f.heldTable(c), &properties), qt.IsNil)
	return properties.Partitioning.Minimum
}

// A starting layout is a create-time setting. YDB keeps no record of it except
// the minimum partition count it sets, so the minimum is what a later
// comparison reads: a declaration that restates the layout the table was
// created with is no change, and one that asks for another is refused with the
// reason.
func TestStartingLayoutIsCreatedAndRefusedOnAnExistingTableSubprocess(t *testing.T) {
	unsigned := []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "body", Type: "Utf8", Nullable: true}}
	for _, test := range []struct {
		name    string
		columns []*protocol.DeclaredColumn
		key     []string
		fields  string
		// minimum is what the table reports after creation.
		minimum string
		// other is a layout the table was not created with.
		other string
		// otherWords are the words of the refusal of that layout.
		otherWords string
	}{
		{"a count of uniform partitions", unsigned, []string{"id"}, `"uniformPartitions":"4"`, "4",
			`"uniformPartitions":"8"`, "it declares UNIFORM_PARTITIONS = 8, which YDB takes only when it creates a table"},
		{"split points on the first key column", unsigned, []string{"id"}, `"partitionAtKeys":[{"values":["10"]},{"values":["20"]}]`, "3",
			`"partitionAtKeys":[{"values":["10"]}]`, "it declares PARTITION_AT_KEYS with one split point, which YDB takes only when it creates a table"},
		{"split points over both columns of a composite key", []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "body", Type: "Utf8", Primary: true}},
			[]string{"id", "body"}, `"partitionAtKeys":[{"values":["10","a"]},{"values":["20"]}]`, "3",
			`"uniformPartitions":"2"`, "it declares UNIFORM_PARTITIONS = 2, which YDB takes only when it creates a table"},
		{"split points over a signed integer key", []*protocol.DeclaredColumn{{Name: "id", Type: "Int32", Primary: true}, {Name: "body", Type: "Utf8", Nullable: true}},
			[]string{"id"}, `"partitionAtKeys":[{"values":["5"]}]`, "2", `"partitionAtKeys":[{"values":["5"]},{"values":["6"]}]`,
			"it declares PARTITION_AT_KEYS with 2 split points, which YDB takes only when it creates a table"},
		{"split points over a text key", []*protocol.DeclaredColumn{{Name: "id", Type: "Utf8", Primary: true}, {Name: "body", Type: "Utf8", Nullable: true}},
			[]string{"id"}, `"partitionAtKeys":[{"values":["m"]}]`, "2", `"uniformPartitions":"3"`, "it declares UNIFORM_PARTITIONS = 3, which YDB takes only when it creates a table"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := layoutFixture(c, test.columns, test.key, test.fields)
			c.Assert(fixture.heldMinimum(c), qt.Equals, test.minimum)
			table := `{"primaryKey":` + mustJSON(c, test.key) + `,`
			restated := desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, table+test.fields+`}`)
			other := desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, table+test.other+`}`)

			c.Assert(fixture.compareRecords(c, restated).Changes, qt.HasLen, 0)
			plan := fixture.plan(c, nil, restated)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)

			found := fixture.compareRecords(c, other)
			c.Assert(found.Changes, qt.HasLen, 1)
			refused := fixture.plan(c, found.Changes, other)
			c.Assert(refused.Diagnostics, qt.HasLen, 1)
			c.Assert(refused.Diagnostics[0].Message, qt.Satisfies, func(message string) bool { return strings.Contains(message, test.otherWords) })
			c.Assert(refused.Contributions, qt.HasLen, 0)
		})
	}
}

func mustJSON(c *qt.C, value any) string {
	c.Helper()
	encoded, err := json.Marshal(value)
	c.Assert(err, qt.IsNil)
	return string(encoded)
}
