package objectplan_test

import (
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/objectplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// scheme is the table or view of kind at directory/name, as the host names
// one in a common step and a table outline: without the database.
func scheme(kind, directory, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: kind, Schema: testcomparison.Part(directory), Name: testcomparison.Part(name)}
}

// nestedRequest is a planning request of the database /local whose current
// objects claim the root of each nested database in roots, as a read claims
// it, and that creates the tables and views of steps through common steps,
// the tables of outlines, and the native changes.
func nestedRequest(roots []string, steps, outlines []*protocol.ComparisonIdentity, changes ...objectChange) *protocol.PlanFeaturesRequest {
	request := conflictRequest(changes...)
	request.DatabasePath = "/local"
	source := &protocol.ComparisonKnowledgeSource{Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}
	for _, root := range roots {
		source.Regions = append(source.Regions, &protocol.ComparisonRegion{Scope: &protocol.ComparisonNamespace{Schema: testcomparison.Part(root), SchemaSubtree: true},
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE, Reason: schema.NestedDatabaseReason(root)}})
	}
	request.CurrentObjects.Coverage = &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{
		{Model: &protocol.ValueModel{Kind: schema.TopicProperties}, Sources: []*protocol.ComparisonKnowledgeSource{source}},
	}}
	for _, subject := range steps {
		request.CommonSteps = append(request.CommonSteps, &protocol.CommonPlanningStep{Metadata: &protocol.PlanningMetadata{
			Effects: []*protocol.PlanningEffect{{Subject: subject, Action: protocol.PlanningEffect_CREATE}}}})
	}
	for _, subject := range outlines {
		request.Tables = append(request.Tables, &protocol.PlanningTable{Subject: subject, Action: protocol.PlanningTable_CREATE})
	}
	return request
}

// A plan that creates nothing inside a nested database is left alone: a
// creation beside one, in a directory whose name only starts with the root's,
// a drop below a nested database's path, and any creation where the read
// claims no nested database.
func TestNestedDatabaseCreations_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name            string
		roots           []string
		steps, outlines []*protocol.ComparisonIdentity
		changes         []objectChange
	}{
		{name: "a table beside the nested database", roots: []string{"db2"}, steps: []*protocol.ComparisonIdentity{scheme("table", "app", "items")},
			outlines: []*protocol.ComparisonIdentity{scheme("table", "app", "items")}},
		{name: "a table in a directory whose name starts with the root's", roots: []string{"db2"},
			steps: []*protocol.ComparisonIdentity{scheme("table", "db22", "items")}},
		{name: "a native object beside the nested database", roots: []string{"db2"},
			changes: []objectChange{created(object(schema.TopicProperties, "app/db2", "events"))}},
		{name: "a native object the plan drops", roots: []string{"db2"},
			changes: []objectChange{dropped(object(schema.TopicProperties, "db2", "events"))}},
		{name: "a read that claims no nested database", steps: []*protocol.ComparisonIdentity{scheme("table", "db2", "items")},
			changes: []objectChange{created(object(schema.TopicProperties, "db2", "events"))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			err := objectplan.NestedDatabaseCreations(nestedRequest(test.roots, test.steps, test.outlines, test.changes...))

			c.Assert(err, qt.IsNil)
		})
	}
}

// A plan that creates a table, a view or a native object at or below the root
// of a nested database is refused in words that name the nested database:
// YDB runs the creation through the outer session and writes the object into
// the nested database, where no later read of the outer one finds it.
func TestNestedDatabaseCreations_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name            string
		steps, outlines []*protocol.ComparisonIdentity
		changes         []objectChange
		want            string
	}{
		{name: "a table a common step creates", steps: []*protocol.ComparisonIdentity{scheme("table", "db2", "items")},
			want: "this plan creates the table db2/items inside /local/db2"},
		{name: "a table the plan outlines below the root", outlines: []*protocol.ComparisonIdentity{scheme("table", "db2/inner", "items")},
			want: "this plan creates the table db2/inner/items inside /local/db2"},
		{name: "a view a common step creates", steps: []*protocol.ComparisonIdentity{scheme("view", "db2", "recent")},
			want: "this plan creates the view db2/recent inside /local/db2"},
		{name: "a table at the root's own path", steps: []*protocol.ComparisonIdentity{scheme("table", "", "db2")},
			want: "this plan creates the table db2 inside /local/db2"},
		{name: "a native object the plan creates", changes: []objectChange{created(object(schema.TopicProperties, "db2", "events"))},
			want: "this plan creates the object db2/events inside /local/db2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			err := objectplan.NestedDatabaseCreations(nestedRequest([]string{"db2"}, test.steps, test.outlines, test.changes...))

			c.Assert(err, qt.ErrorMatches, regexp.QuoteMeta(test.want)+
				`, which is another YDB database nested in /local: a session of /local neither reads nor drops what it holds\. .*`)
		})
	}
}
