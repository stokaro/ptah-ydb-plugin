package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// dropRequest plans the removal of app.records, which holds (id, payload)
// keyed by id, with the host's step that drops it.
func dropRequest(c *qt.C) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := alterRequest(c, alterCase{heldTable: `{"primaryKey":["id"]}`, held: []string{"payload"}})
	table := request.Tables[0]
	table.Action, table.Desired = protocol.PlanningTable_DROP, nil
	table.Current.Columns[0].PrimaryKey = true
	request.CommonSteps = []*protocol.CommonPlanningStep{commonStep("drop-records", table.Subject, "",
		&protocol.PlanningEffect{Subject: proto.CloneOf(table.Subject), Action: protocol.PlanningEffect_DROP})}
	return request
}

func TestDropPlan_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := dropRequest(c)
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), request, plan), qt.IsNil)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	step := plan.Contributions[0].Steps[0]
	c.Assert(step.Id.Name, qt.Equals, "table-drop/000000")
	c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.DropOperationKind)
	c.Assert(string(step.Operation.Value.Value.Payload), qt.Equals, `{"columns":["id","payload"],"key":["id"],"store":"row"}`)
	c.Assert(step.Metadata.Impact, qt.Equals, protocol.ChangeSemantics_DESTRUCTIVE)
	c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_DROP)
	c.Assert(plan.Rewrites, qt.HasLen, 1)
	c.Assert(plan.Rewrites[0].Sources[0].Name, qt.Equals, "drop-records")
	c.Assert(plan.Rewrites[0].Replacement.Name, qt.Equals, step.Id.Name)
	c.Assert(plan.Parents, qt.HasLen, len(tablecreate.ParentKinds()))
	c.Assert(plan.Parents[0].Action, qt.Equals, protocol.PlanningTable_DROP)

	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	native, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(native.Units[0].GetNative().Kind, qt.Equals, tablecreate.DropOperationKind)
	rendering.Form = protocol.RenderRequest_TEXT
	text, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(text.Units[0].GetSql(), qt.Equals, "DROP TABLE `app/records`;")
}

// The removal of a table its properties read keyed in another order keeps
// that order, and a column table's is held in the column store.
func TestCaptureDropKeepsTheKeyAndTheStore(t *testing.T) {
	c := testcheck.New(t)
	table := dropRequest(c).Tables[0]
	table.Current.Columns = append(table.Current.Columns, &protocol.ObservedColumn{Name: "second", PrimaryKey: true})
	table.Current.Features.Facets.Records[0] = observedFacet(c, schema.TableProperties, "", `{"primaryKey":["second","id"]}`)
	value, err := tablecreate.CaptureDrop("drop", owner(), table.Current)
	c.Assert(err, qt.IsNil)
	drop, err := tablecreate.DecodeDrop(value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(drop, qt.DeepEquals, &tablecreate.Drop{Store: tablecreate.RowStore, Columns: []string{"id", "payload", "second"}, Key: []string{"second", "id"}})
}

func TestDropPlan_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	// The SDK refuses a removal without the read of the table it drops.
	request := dropRequest(c)
	request.Tables[0].Current = nil
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.ErrorMatches, `.*planning table action disagrees with captured state`)
	c.Assert(plan, qt.IsNil)

	request = dropRequest(c)
	request.Tables[0].Current.Columns = nil
	plan, err = backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, `table "app/records": a removal checks the table it drops against the read of it, and the table was not read`)
	c.Assert(plan.Contributions, qt.HasLen, 0)

	request = dropRequest(c)
	request.Capabilities["transactional_ddl"] = true
	plan, err = backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics[0].Message, qt.Equals, "YDB table removal requires established nontransactional DDL")
}

// replicatedFeed is a changefeed an asynchronous replication controls, which
// writes the replica at path.
func replicatedFeed(path string) string {
	return `{"changefeeds":[{"description":{"name":"updates","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED",` +
		`"attributes":{"__async_replication":"{\"path\":\"` + path + `\",\"id\":\"1\",\"supports_topic_autopartitioning\":false}"}},"topic":{"partitioningSettings":{}}}]}`
}

// A table that holds a changefeed an asynchronous replication of another
// database controls is not dropped: YDB drops it and leaves the replication
// failing (`Discovery for all topics failed`), measured on 25.1.4.7 and
// 26.2.1.14, and no plan of this database drops the replication, as the
// host's embedded engine refused it. A replication lives in its replicas'
// database. Ported from the host's
// TestParentOperationsRefuseReplicationBindingsWithoutChildChanges.
func TestDropPlanRefusesATableAReplicationOfAnotherDatabaseReads(t *testing.T) {
	for _, test := range []struct {
		name, database, replica string
	}{
		{name: "a replica of another database", database: "/local", replica: "/other/replica/records"},
		{name: "a replica at a sibling of the database", database: "/local", replica: "/localother/records"},
		{name: "a session that names no database", replica: "/local/replica/records"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := dropRequest(c)
			request.DatabasePath = test.database
			request.Tables[0].Current.Features.Facets.Records = append(request.Tables[0].Current.Features.Facets.Records,
				observedFacet(c, schema.ChangefeedProperties, "", replicatedFeed(test.replica)))

			plan, err := backend.New().PlanFeatures(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, `table "app/records": changefeed "updates" is a replication-managed stream that depends on this table; `+
				`its replication's controller must release it before the table is dropped, and the replication, which writes `+
				test.replica+`, lives in another database; drop it there first, or keep the table`)
		})
	}
}

// A table a replication of the database reads is left to the planner of
// replications, which reads whether the plan keeps the replication: a plan
// that drops the replication drops it first, with CASCADE, which releases the
// stream, and the table goes after it.
func TestDropPlanDropsATableAReplicationOfTheDatabaseReads(t *testing.T) {
	c := testcheck.New(t)
	request := dropRequest(c)
	request.DatabasePath = "/local"
	request.Tables[0].Current.Features.Facets.Records = append(request.Tables[0].Current.Features.Facets.Records,
		observedFacet(c, schema.ChangefeedProperties, "", replicatedFeed("/local/replica/records")))

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plannedSteps(plan), qt.DeepEquals, []string{"table-drop/000000 table-drop-operation"})
}

// The control: a table whose changefeeds Ptah manages is dropped with them.
func TestDropPlanDropsATableWithItsChangefeeds(t *testing.T) {
	c := testcheck.New(t)
	request := dropRequest(c)
	request.Tables[0].Current.Features.Facets.Records = append(request.Tables[0].Current.Features.Facets.Records, observedFacet(c, schema.ChangefeedProperties, "", heldFeed))
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plannedSteps(plan), qt.DeepEquals, []string{"table-drop/000000 table-drop-operation"})
}

func TestDecodeDrop_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name, payload, want string
	}{
		{name: "an unknown store", payload: `{"store":"heap","columns":["id"],"key":["id"]}`, want: "a YDB table removal names a row or a column store"},
		{name: "no key", payload: `{"store":"row","columns":["id"],"key":[]}`, want: "a YDB table removal states the table's columns and key"},
		{name: "a column twice", payload: `{"store":"row","columns":["id","id"],"key":["id"]}`, want: "a YDB table removal states each column once"},
		{name: "a key outside the columns", payload: `{"store":"row","columns":["id"],"key":["other"]}`, want: "a YDB table removal's key names its columns once each"},
		{name: "an unknown field", payload: `{"store":"row","columns":["id"],"key":["id"],"extra":1}`, want: "invalid YDB table removal payload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			definition := tablecreate.DropModel()
			value := &protocol.Envelope{ProviderId: schema.ProviderID, Kind: tablecreate.DropOperationKind, Owner: owner(), SchemaVersion: 1,
				Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(tc.payload)}
			drop, err := tablecreate.DecodeDrop(value)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(drop, qt.IsNil)
		})
	}
}

// A table created at the path of a directory above a table the request drops
// is refused before anything is planned, in the host's embedded engine's
// words: YDB keeps the directory after the drop, so the creation can never
// apply.
func TestCreationOverADroppedTable_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := creationPlan(c)
	dropped := dropRequest(c).Tables[0]
	dropped.Subject.Schema, dropped.Subject.Name = part("app/records"), part("old")
	request.Tables = append(request.Tables, dropped)

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].GetMessage(), qt.Matches, "this plan drops app/records/old and creates an object at app/records, which is a directory in the database. .*")
	c.Assert(plan.Contributions, qt.HasLen, 0)
}
