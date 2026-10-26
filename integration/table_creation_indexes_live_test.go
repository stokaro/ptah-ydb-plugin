//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// TestTableCreationWithIndexesSubprocess creates a table with a unique, an
// asynchronous and a covering index, by the native call and by the CREATE
// TABLE a migration file holds, and reads both back: each index is built with
// the table, so a unique one is created on a release that refuses one on an
// existing table.
func TestTableCreationWithIndexesSubprocess(t *testing.T) {
	c := qt.New(t)
	opened, directory, database := inspectionFixture(c)
	client := startProviderFor(c, provider.PlanningLifecycleFeature, provider.TextUnitsFeature)
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	create := func(c *qt.C, name string, form protocol.RenderRequest_Form) *protocol.Unit {
		c.Helper()
		c.Cleanup(func() {
			cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			c.Check(execFixtureSchema(cleanup, opened.DB, "DROP TABLE IF EXISTS `"+path.Join(database, directory, name)+"`"), qt.IsNil)
		})
		owner := &protocol.Reference{Engine: "ydb", Kind: "table", Database: database, Schema: directory, Name: name}
		declared := &protocol.DeclaredTable{Name: name, Schema: directory, PrimaryKey: []string{"id"},
			Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "mail", Type: "Utf8", Nullable: true},
				{Name: "note", Type: "Utf8", Nullable: true}, {Name: "seen", Type: "Timestamp", Nullable: true}},
			Indexes: []*protocol.DeclaredIndex{{Name: "by_mail", Fields: []string{"mail"}, Unique: true},
				{Name: "by_note", Fields: []string{"note"}, Type: "async"},
				{Name: "by_seen", Fields: []string{"seen"}, IncludeColumns: []string{"note"}}},
			Features: &protocol.CapturedFeatures{
				Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
				Facets:  &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}}}
		planning := &protocol.PlanFeaturesRequest{Engine: "ydb", PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(), DatabasePath: database,
			Capabilities: session.Capabilities, Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(session.Identifiers)},
			Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(owner), Action: protocol.PlanningTable_CREATE, Desired: declared}}}
		prepareCapturedCreation(c, client, planning)
		plan, err := client.PlanFeatures(c.Context(), planning)
		c.Assert(err, qt.IsNil)
		c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
		rendering := testplanning.RenderRequest(plan, session.Capabilities)
		rendering.Form = form
		rendered, err := client.Render(c.Context(), rendering)
		c.Assert(err, qt.IsNil)
		result, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.Id, RequestId: "create-" + name, Units: rendered.Units})
		c.Assert(err, qt.IsNil)
		c.Assert(result.Outcome, qt.Equals, protocol.Execution_APPLIED, qt.Commentf("%s", result))
		return rendered.Units[0]
	}
	byCall := create(c, "by_call", protocol.RenderRequest_ANY)
	c.Assert(byCall.GetNative(), qt.IsNotNil)
	text := create(c, "by_text", protocol.RenderRequest_TEXT)
	c.Assert(text.GetSql(), qt.Equals, "CREATE TABLE `"+directory+"/by_text` (\n    `id` Uint64 NOT NULL,\n    `mail` Utf8,\n    `note` Utf8,\n    `seen` Timestamp,\n"+
		"    PRIMARY KEY (`id`),\n    INDEX `by_mail` GLOBAL UNIQUE SYNC ON (`mail`),\n    INDEX `by_note` GLOBAL ASYNC ON (`note`),\n"+
		"    INDEX `by_seen` GLOBAL SYNC ON (`seen`) COVER (`note`)\n);")
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	held := map[string][]string{}
	for _, value := range snapshot.Tables {
		for _, facet := range value.Facets {
			if facet.Kind != schema.IndexProperties {
				continue
			}
			var observed native.IndexObservations
			c.Assert(protojson.Unmarshal(facet.Payload, &observed), qt.IsNil)
			for _, index := range observed.Indexes {
				description := index.Description
				kind := description.ProtoReflect().WhichOneof(description.ProtoReflect().Descriptor().Oneofs().ByName("type"))
				held[value.Identity.Name] = append(held[value.Identity.Name], fmt.Sprint(description.Name, " ", kind.Name(), " ", description.Status,
					" ", description.IndexColumns, " ", description.DataColumns))
			}
		}
	}
	c.Assert(held["by_call"], qt.HasLen, 3)
	c.Assert(held["by_text"], qt.DeepEquals, held["by_call"])
}
