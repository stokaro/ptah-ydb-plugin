package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

const (
	noFeeds      = `{"changefeeds":[]}`
	declaredFeed = `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`
	heldFeed     = `{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{}}}]}`
	declaredKeys = `{"changefeeds":[{"name":"f","mode":"MODE_KEYS_ONLY","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`
	declaredLong = `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{},"retentionPeriod":"172800s"}}]}`
	heldRegion   = `{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED","awsRegion":"eu-west-1"},"topic":{"partitioningSettings":{}}}]}`
)

// newest are the changefeed capabilities of a 26.2 session.
func newest() map[string]bool {
	return map[string]bool{changefeeds.CapabilityChangefeeds: true, changefeeds.CapabilityUserSIDs: true, changefeeds.CapabilitySchemaChanges: true,
		changefeeds.CapabilityTopicAutoPartitioning: true, "topic_consumer_availability_period": true}
}

func feedCase(desired, held string) alterCase {
	return alterCase{desiredTable: plainTable, heldTable: plainTable, desiredChangefeeds: desired, heldChangefeeds: held, capabilities: newest()}
}

// plannedStep is what one step says about itself: for the changefeed step,
// the action of each change and the effect on each changefeed.
type plannedStep struct {
	Name    string
	Actions []changefeeds.Action
	Effects []string
	Impact  protocol.ChangeSemantics_Impact
}

// planned plans a request and reads back each step it contributes.
func planned(c *qt.C, request *protocol.PlanFeaturesRequest) ([]plannedStep, *protocol.PlannedFeatures) {
	c.Helper()
	original := proto.CloneOf(request)
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(request, original), qt.IsTrue)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	var steps []plannedStep
	for _, contribution := range plan.Contributions {
		for _, step := range contribution.Steps {
			value := step.Operation.Value.Value
			if value.Kind != schema.ChangefeedOperation {
				steps = append(steps, plannedStep{Name: step.Id.Name})
				continue
			}
			decoded, err := schema.DecodeChangefeedOperation(value)
			c.Assert(err, qt.IsNil)
			c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
			c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_READ)
			result := plannedStep{Name: step.Id.Name, Impact: step.Metadata.Impact}
			for _, change := range decoded {
				result.Actions = append(result.Actions, change.Action())
			}
			for _, effect := range step.Metadata.Effects[1:] {
				c.Assert(effect.Subject.Kind, qt.Equals, "ydb.changefeed")
				result.Effects = append(result.Effects, effect.Subject.Name.Normalized+":"+effect.Action.String())
			}
			steps = append(steps, result)
		}
	}
	return steps, plan
}

// dependencies lists each edge of a plan as its two step names.
func dependencies(plan *protocol.PlannedFeatures) [][2]string {
	var edges [][2]string
	for _, contribution := range plan.Contributions {
		for _, dependency := range contribution.Dependencies {
			edges = append(edges, [2]string{dependency.Before.Name, dependency.After.Name})
		}
	}
	return edges
}

func TestChangefeedPlan_HappyPath(t *testing.T) {
	unstated := feedCase("", heldFeed)
	unstated.changefeedKnowledge = protocol.ModelKnowledge_COMPLETE
	swap := feedCase(`{"changefeeds":[{"name":"a","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`, heldFeed)
	const step = "row-table-changefeeds/000000"
	for _, test := range []struct {
		name string
		in   alterCase
		want plannedStep
	}{
		{"adds a changefeed", feedCase(declaredFeed, noFeeds),
			plannedStep{Name: step, Actions: []changefeeds.Action{changefeeds.Add}, Effects: []string{"f:CREATE"}, Impact: protocol.ChangeSemantics_ADDITIVE}},
		{"drops a changefeed the declaration leaves out", feedCase(noFeeds, heldFeed),
			plannedStep{Name: step, Actions: []changefeeds.Action{changefeeds.Drop}, Effects: []string{"f:DROP"}, Impact: protocol.ChangeSemantics_DESTRUCTIVE}},
		{"drops a changefeed of a table the source states none for", unstated,
			plannedStep{Name: step, Actions: []changefeeds.Action{changefeeds.Drop}, Effects: []string{"f:DROP"}, Impact: protocol.ChangeSemantics_DESTRUCTIVE}},
		{"drops and adds a changefeed whose mode changes in one step", feedCase(declaredKeys, heldFeed),
			plannedStep{Name: step, Actions: []changefeeds.Action{changefeeds.Recreate}, Effects: []string{"f:ALTER"}, Impact: protocol.ChangeSemantics_DESTRUCTIVE}},
		{"changes the topic in place", feedCase(declaredLong, heldFeed),
			plannedStep{Name: step, Actions: []changefeeds.Action{changefeeds.Topic}, Effects: []string{"f:ALTER"}, Impact: protocol.ChangeSemantics_BEHAVIORAL}},
		{"swaps two changefeeds in one step", swap,
			plannedStep{Name: step, Actions: []changefeeds.Action{changefeeds.Add, changefeeds.Drop}, Effects: []string{"a:CREATE", "f:DROP"}, Impact: protocol.ChangeSemantics_DESTRUCTIVE}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			steps, plan := planned(c, alterRequest(c, test.in))
			c.Assert(steps, qt.DeepEquals, []plannedStep{test.want})
			c.Assert(dependencies(plan), qt.HasLen, 0)
			for _, parent := range plan.Parents {
				c.Assert(parent.Receipt.Steps, qt.HasLen, 1, qt.Commentf(parent.Receipt.Kind))
				c.Assert(parent.Receipt.Steps[0].Name, qt.Equals, step)
			}
		})
	}
}

// TestChangefeedPlan_LeavesThemAlone plans no changefeed step where the
// declaration does not manage the changefeeds, where they already match, and
// where the comparison leaves the move undecided.
func TestChangefeedPlan_LeavesThemAlone(t *testing.T) {
	uninspected := feedCase("", heldFeed)
	uninspected.changefeedKnowledge = protocol.ModelKnowledge_UNINSPECTED
	for _, test := range []struct {
		name string
		in   alterCase
	}{
		{"a source that could not describe changefeeds", uninspected},
		{"a source that states nothing about them", feedCase("", heldFeed)},
		{"the declared changefeeds", feedCase(declaredFeed, heldFeed)},
		{"a changefeed the read cannot represent", feedCase(declaredFeed, heldRegion)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			steps, plan := planned(c, alterRequest(c, test.in))
			c.Assert(steps, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// TestChangefeedPlan_FailurePath refuses a changefeed the target cannot hold,
// with the host's words.
func TestChangefeedPlan_FailurePath(t *testing.T) {
	sids := `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","userSids":true,"topic":{"partitioningSettings":{}}}]}`
	partitioned := `{"changefeeds":[{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{"minActivePartitions":"2"}}}]}`
	withoutChangefeeds := feedCase(declaredFeed, noFeeds)
	withoutChangefeeds.capabilities = map[string]bool{}
	oldLine := feedCase(sids, noFeeds)
	oldLine.capabilities = map[string]bool{changefeeds.CapabilityChangefeeds: true}
	notCaptured := feedCase(declaredFeed, "")
	clash := feedCase(`{"changefeeds":[{"name":"by_label","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`, noFeeds)
	clash.declared, clash.held = []string{"label"}, []string{"label"}
	clash.desiredIndexes = declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, nil))
	clash.heldIndexes = heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil))
	for _, test := range []struct {
		name string
		in   alterCase
		// keyType is the type of the key column id.
		keyType string
		wantErr string
	}{
		{"a target without changefeeds", withoutChangefeeds, "Uint64",
			`changefeed "f" of table "app/records", which requires target capability changefeeds, unavailable on this ydb target`},
		{"USER_SIDS on a line without it", oldLine, "Uint64",
			`changefeed "f" of table "app/records" takes USER_SIDS, which requires target capability changefeed_user_sids, unavailable on this ydb target`},
		{"a partitioned topic on a key YDB does not split", feedCase(partitioned, noFeeds), "Utf8",
			`changefeed "f" of table "app/records": its topic starts with 2 partitions, which YDB splits by the first key column, and takes that column only as Uint32 or Uint64, not Utf8`},
		{"a changefeed named after an index", clash, "Uint64",
			`table "app/records": changefeed "by_label" has the name of one of its indexes, and YDB keeps both under the table's path`},
		{"changefeeds the read did not capture", notCaptured, "Uint64", `The table's current changefeeds were not captured`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := alterRequest(c, test.in)
			request.Tables[0].Desired.Columns[0].Type = test.keyType
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Code, qt.Equals, protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, test.wantErr)
		})
	}
}

// TestChangefeedPlan_RefusesANameAnIndexFrees refuses a changefeed and an
// index that trade a name in one plan: the host adds before it drops, so the
// addition would meet the object it replaces.
func TestChangefeedPlan_RefusesANameAnIndexFrees(t *testing.T) {
	takes := feedCase(`{"changefeeds":[{"name":"by_label","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}}]}`, noFeeds)
	takes.declared, takes.held = []string{"label"}, []string{"label"}
	takes.desiredIndexes, takes.heldIndexes = declaredIndexes(), heldIndexes(indexJSON("globalIndex", "by_label", byLabel, nil))
	frees := feedCase(noFeeds, `{"changefeeds":[{"description":{"name":"by_label","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{}}}]}`)
	frees.declared, frees.held = []string{"label"}, []string{"label"}
	frees.desiredIndexes, frees.heldIndexes = declaredIndexes(indexJSON("globalIndex", "by_label", byLabel, nil)), heldIndexes()
	for _, test := range []struct {
		name    string
		in      alterCase
		wantErr string
	}{
		{"a changefeed takes the name of an index the plan drops", takes,
			`changefeed "by_label" of table "app/records" takes the name of an index the plan drops, .*`},
		{"an index takes the name of a changefeed the plan drops", frees,
			`changefeed "by_label" of table "app/records" has the name of an index the plan adds, .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, test.in))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, test.wantErr)
		})
	}
}

// TestChangefeedPlan_NewTable adds the changefeeds of a new table once the
// table exists: CREATE TABLE takes no changefeed.
func TestChangefeedPlan_NewTable(t *testing.T) {
	c := qt.New(t)
	request := creationPlan(c)
	request.Capabilities = newest()
	request.Capabilities["transactional_ddl"] = false
	table := request.Tables[0].Desired
	table.Features.Facets.Records = append(table.Features.Facets.Records, facet(c, schema.ChangefeedProperties, "",
		`{"changefeeds":[{"name":"b","mode":"MODE_UPDATES","format":"FORMAT_JSON","topic":{"partitioningSettings":{}}},`+
			`{"name":"a","mode":"MODE_NEW_IMAGE","format":"FORMAT_JSON","topic":{"partitioningSettings":{},"consumers":[{"name":"c"}]}}]}`))
	steps, plan := planned(c, request)
	c.Assert(steps, qt.DeepEquals, []plannedStep{
		{Name: "row-table-create/000000"},
		{Name: "row-table-changefeeds/000000", Actions: []changefeeds.Action{changefeeds.Add, changefeeds.Add}, Effects: []string{"a:CREATE", "b:CREATE"},
			Impact: protocol.ChangeSemantics_ADDITIVE},
	})
	c.Assert(dependencies(plan), qt.DeepEquals, [][2]string{{"row-table-create/000000", "row-table-changefeeds/000000"}})
	for _, parent := range plan.Parents {
		c.Assert(parent.Receipt.Steps, qt.HasLen, 2)
	}
}

// A column table takes no changefeed.
func TestChangefeedPlan_ColumnTable(t *testing.T) {
	c := qt.New(t)
	capabilities := newest()
	capabilities["transactional_ddl"] = false
	request := columnCreationRequest(c, `{"hashColumns":["id"]}`, capabilities)
	table := request.Tables[0].Desired
	table.Features.Facets.Records = append(table.Features.Facets.Records, facet(c, schema.ChangefeedProperties, "", declaredFeed))
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `.*row-table partitioning, column families and changefeeds cannot be applied to a column table`)
}
