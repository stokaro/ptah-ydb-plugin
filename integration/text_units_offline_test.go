//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/protobuf/proto"
)

// offlineCreation plans the creation of app.records offline through client,
// with the table settings payload states, and returns its render request.
func offlineCreation(c *qt.C, client *providerruntime.Client, payload string) *protocol.RenderRequest {
	c.Helper()
	owner := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}
	features := &protocol.CapturedFeatures{
		Objects: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}},
		Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{},
			Records: []*protocol.ComparisonRecord{desiredFacet(c, client, owner, schema.TableProperties, payload)}},
	}
	request := &protocol.PlanFeaturesRequest{Engine: "ydb", TargetFeatures: []string{provider.PlanningLifecycleFeature}, PlanningId: tablecreate.ServiceID, ParentKinds: tablecreate.ParentKinds(),
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(client.Description().Engines[0].Identifiers)},
		Capabilities: map[string]bool{"transactional_ddl": false},
		Tables: []*protocol.PlanningTable{{Subject: testplanning.Subject(owner), Action: protocol.PlanningTable_CREATE,
			Desired: &protocol.DeclaredTable{Name: owner.Name, Schema: owner.Schema, PrimaryKey: []string{"id"},
				Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Uint64", Primary: true}, {Name: "ts", Type: "Timestamp", Nullable: true}}, Features: features}}}}
	prepareCapturedCreation(c, client, request)
	projectCapturedCreation(c, client, request)
	plan, err := client.PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	rendering := testplanning.RenderRequest(plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	return rendering
}

// The TEXT form is a two-way addition of contract 25.3: a host that lists
// render.text-units gets text units, and one that does not cannot ask for
// them. No database session is opened.
func TestTextUnitsOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	listing := startProviderFor(c, provider.PlanningLifecycleFeature, provider.TextUnitsFeature)
	c.Assert(listing.Description().Engines[0].Features, qt.Contains, provider.TextUnitsFeature)
	rendering := offlineCreation(c, listing, `{"primaryKey":["id"],"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}}`)
	rendered, err := listing.Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(rendered.Units[0].GetSql(), qt.Equals, "CREATE TABLE `app/records` (\n    `id` Uint64 NOT NULL,\n    `ts` Timestamp,\n"+
		"    PRIMARY KEY (`id`)\n) WITH (TTL = Interval(\"PT1H\") ON `ts`);")

	// A change only the native call makes refuses the batch, through the SDK's
	// checks on both ends.
	refusing := offlineCreation(c, listing, `{"primaryKey":["id"],"attributes":{"purpose":"native-only"}}`)
	refused, err := listing.Render(c.Context(), refusing)
	c.Assert(err, qt.IsNil)
	c.Assert(refused.Complete, qt.IsFalse)
	c.Assert(refused.Units, qt.HasLen, 0)
	c.Assert(refused.Refusal.Reason, qt.Equals, protocol.RenderRefusal_NO_TEXT_FORM)
	c.Assert(refused.Refusal.ChangeId, qt.Equals, refusing.Changes[0].Id)
	c.Assert(refused.Refusal.Message, qt.Not(qt.Contains), "native-only")

	// A host of an earlier minor, or one that does not act on text units,
	// negotiates no TEXT form: the client refuses the request before it is
	// sent, and the ANY form renders the native call.
	unlisted := startProviderFor(c, provider.PlanningLifecycleFeature)
	c.Assert(unlisted.Description().Engines[0].Features, qt.Not(qt.Contains), provider.TextUnitsFeature)
	rendering = offlineCreation(c, unlisted, `{"primaryKey":["id"]}`)
	refused, err = unlisted.Render(c.Context(), rendering)
	c.Assert(err, qt.ErrorMatches, `.*render\.text-units was not negotiated`)
	c.Assert(refused, qt.IsNil)
	rendering.Form = protocol.RenderRequest_ANY
	rendered, err = unlisted.Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units[0].GetNative().GetKind(), qt.Equals, tablecreate.OperationKind)
}
