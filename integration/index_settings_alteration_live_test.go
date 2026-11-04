//go:build integration

package integration_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/encoding/protojson"
)

// indexWithSettings states the index by_payload of the given kind on payload,
// covering cover, with the given settings fields.
func indexWithSettings(kind, cover, settings string) string {
	text := `"name":"by_payload","indexColumns":["payload"],"` + kind + `":{"settings":{` + settings + `}}`
	if cover != "" {
		text += `,"dataColumns":` + cover
	}
	return text
}

// heldIndexSettings is the partitioning and read replicas the provider reads
// back for by_payload, as JSON.
func (f *facetFixture) heldIndexSettings(c *qt.C) string {
	c.Helper()
	held := f.heldIndexes(c)["by_payload"]
	c.Assert(held, qt.IsNotNil)
	encoded, err := protojson.Marshal(tablecreate.HeldIndexSettings(held))
	c.Assert(err, qt.IsNil)
	return string(encoded)
}

// decodedJSON is text decoded, for qt.JSONEquals to compare with.
func decodedJSON(c *qt.C, text string) any {
	c.Helper()
	var value any
	c.Assert(json.Unmarshal([]byte(text), &value), qt.IsNil)
	return value
}

// applyIndexSettings compares the declared index set with the table, plans and
// runs the one step the change takes, and requires nothing left to do after.
func (f *facetFixture) applyIndexSettings(c *qt.C, id, declared string) {
	c.Helper()
	found := f.compareIndexes(c, declared)
	c.Assert(found.Changes, qt.HasLen, 1)
	c.Assert(found.Changes[0].Kind, qt.Equals, schema.IndexProperties)
	plan := f.plan(c, found.Changes, desiredFacet(c, f.client, f.owner, schema.IndexProperties, declared))
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps[0].Id.Name, qt.Equals, "row-table-index-settings/000000")
	c.Assert(f.execute(c, id, plan).Outcome, qt.Equals, protocol.Execution_APPLIED)
	c.Assert(f.compareIndexes(c, declared).Changes, qt.HasLen, 0)
}

func TestIndexSettingsAlterationSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, ", INDEX by_payload GLOBAL SYNC ON (payload)", "")
	// Each step starts from what the previous one left, and states one change.
	// want is the whole of what the index holds after it: a setting the
	// declaration leaves out keeps its value, including the minimum that
	// switching partitioning by load or by size on would reset if the
	// statement did not name it.
	for _, step := range []struct {
		name     string
		settings string
		want     string
	}{
		{"raises the minimum partition count", `"partitioningSettings":{"minPartitionsCount":"3"}`,
			`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"3"}}`},
		{"switches partitioning by load on with a maximum", `"partitioningSettings":{"partitioningByLoad":"ENABLED","maxPartitionsCount":"9"}`,
			`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"3","maxPartitionsCount":"9"}}`},
		{"sets read replicas", `"readReplicasSettings":{"perAzReadReplicasCount":"1"}`,
			`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"3","maxPartitionsCount":"9"},` +
				`"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`},
		{"switches partitioning by size off", `"partitioningSettings":{"partitioningBySize":"DISABLED"}`,
			`{"partitioningSettings":{"partitioningBySize":"DISABLED","partitioningByLoad":"ENABLED","minPartitionsCount":"3","maxPartitionsCount":"9"},` +
				`"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`},
		{"switches partitioning by size on again", `"partitioningSettings":{"partitioningBySize":"ENABLED"}`,
			`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"ENABLED","minPartitionsCount":"3","maxPartitionsCount":"9"},` +
				`"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`},
	} {
		t.Run(step.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture.applyIndexSettings(c, "index-settings-"+step.name, declaredIndexes(indexWithSettings("globalIndex", "", step.settings)))
			c.Assert(fixture.heldIndexSettings(c), qt.JSONEquals, decodedJSON(c, step.want))
		})
	}
	// Replicas declared away leave none: the server reads them back as a
	// count of zero or as none.
	fixture.applyIndexSettings(c, "index-settings-replicas-away", declaredIndexes(indexWithSettings("globalIndex", "", `"readReplicasSettings":{"anyAzReadReplicasCount":"0"}`)))
	held := tablecreate.HeldIndexSettings(fixture.heldIndexes(c)["by_payload"])
	c.Assert(tablecreate.SameReplicas(held.GetReadReplicasSettings(), nil), qt.IsTrue)
	// A declaration that states no settings changes nothing.
	c.Assert(fixture.compareIndexes(c, declaredIndexes(indexSpec("by_payload", "globalIndex", `["payload"]`, ""))).Changes, qt.HasLen, 0)
}

func TestIndexSettingsAlterationKindsSubprocess(t *testing.T) {
	for _, test := range []struct {
		name, clause, kind, cover, field string
	}{
		{"an async covering index", ", INDEX by_payload GLOBAL ASYNC ON (payload) COVER (ts)", "globalAsyncIndex", `["ts"]`, "global_async_index"},
		{"a unique index", ", INDEX by_payload GLOBAL UNIQUE SYNC ON (payload)", "globalUniqueIndex", "", "global_unique_index"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := newFacetFixture(c, test.clause, "")
			declared := declaredIndexes(indexWithSettings(test.kind, test.cover, `"partitioningSettings":{"minPartitionsCount":"2"},"readReplicasSettings":{"anyAzReadReplicasCount":"1"}`))
			fixture.applyIndexSettings(c, "index-settings-kind", declared)
			held := fixture.heldIndexes(c)["by_payload"]
			description := held.ProtoReflect()
			c.Assert(string(description.WhichOneof(description.Descriptor().Oneofs().ByName("type")).Name()), qt.Equals, test.field)
			c.Assert(fixture.heldIndexSettings(c), qt.JSONEquals, decodedJSON(c,
				`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"2"},`+
					`"readReplicasSettings":{"anyAzReadReplicasCount":"1"}}`))
		})
	}
}

func TestIndexSettingsUnchangedSubprocess(t *testing.T) {
	for _, test := range []struct {
		name     string
		settings string
	}{
		{"the settings YDB gives a new index", `"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","minPartitionsCount":"1"}`},
		{"no read replicas", `"readReplicasSettings":{"perAzReadReplicasCount":"0"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := newFacetFixture(c, ", INDEX by_payload GLOBAL SYNC ON (payload)", "")
			c.Assert(fixture.compareIndexes(c, declaredIndexes(indexWithSettings("globalIndex", "", test.settings))).Changes, qt.HasLen, 0)
		})
	}
}

// TestIndexSettingsStalePlanSubprocess plans a minimum, lets another plan
// change the index first, and runs the first plan: the index no longer holds
// the settings the plan captured, so the statement is refused before it runs.
func TestIndexSettingsStalePlanSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, ", INDEX by_payload GLOBAL SYNC ON (payload)", "")
	declared := declaredIndexes(indexWithSettings("globalIndex", "", `"partitioningSettings":{"minPartitionsCount":"3"}`))
	stale := fixture.plan(c, fixture.compareIndexes(c, declared).Changes, desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, declared))
	c.Assert(stale.Diagnostics, qt.HasLen, 0)
	fixture.applyIndexSettings(c, "index-settings-first", declaredIndexes(indexWithSettings("globalIndex", "", `"partitioningSettings":{"minPartitionsCount":"4"}`)))
	execution, err := fixture.tryExecute(c, "index-settings-stale", stale)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	c.Assert(fixture.heldIndexSettings(c), qt.JSONEquals, decodedJSON(c,
		`{"partitioningSettings":{"partitioningBySize":"ENABLED","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","minPartitionsCount":"4"}}`))
}
