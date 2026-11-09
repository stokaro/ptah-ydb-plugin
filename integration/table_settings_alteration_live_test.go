//go:build integration

package integration_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// heldReadSettings are the key bloom filter and the read replicas the provider
// reads back, spelled as the declaration takes them: the filter on or off, an
// unspecified filter being off, and the replicas as PER_AZ:<n> or ANY_AZ:<n>,
// or none for a count of zero.
func (f *facetFixture) heldReadSettings(c *qt.C) (bloom, replicas string) {
	c.Helper()
	var properties struct {
		Bloom    string `json:"keyBloomFilter"`
		Replicas struct {
			PerAZ *string `json:"perAzReadReplicasCount"`
			AnyAZ *string `json:"anyAzReadReplicasCount"`
		} `json:"readReplicasSettings"`
	}
	c.Assert(json.Unmarshal(f.heldTable(c), &properties), qt.IsNil)
	bloom = "off"
	if properties.Bloom == "ENABLED" {
		bloom = "on"
	}
	switch {
	case properties.Replicas.PerAZ != nil && *properties.Replicas.PerAZ != "0":
		replicas = "PER_AZ:" + *properties.Replicas.PerAZ
	case properties.Replicas.AnyAZ != nil && *properties.Replicas.AnyAZ != "0":
		replicas = "ANY_AZ:" + *properties.Replicas.AnyAZ
	default:
		replicas = "none"
	}
	return bloom, replicas
}

func settingsDeclaration(fields string) string {
	return `{"primaryKey":["id"],` + fields + `}`
}

func TestFacetComparisonTableSettingsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, "", "")
	// Each step starts from what the previous one left in the table. The
	// partitioning is never declared, so it must stay as the table holds it.
	for _, step := range []struct {
		name            string
		declared        string
		bloom, replicas string
	}{
		{"turns the key bloom filter on", settingsDeclaration(`"keyBloomFilter":"ENABLED"`), "on", "none"},
		{"sets read replicas and keeps the filter", settingsDeclaration(`"readReplicasSettings":{"perAzReadReplicasCount":"1"}`), "on", "PER_AZ:1"},
		{"changes the replica mode", settingsDeclaration(`"readReplicasSettings":{"anyAzReadReplicasCount":"1"}`), "on", "ANY_AZ:1"},
		{"turns the filter off and keeps the replicas", settingsDeclaration(`"keyBloomFilter":"DISABLED"`), "off", "ANY_AZ:1"},
		{"removes the replicas declared as PER_AZ:0", settingsDeclaration(`"readReplicasSettings":{"perAzReadReplicasCount":"0"}`), "off", "none"},
		{"sets the filter and the replicas together",
			settingsDeclaration(`"keyBloomFilter":"ENABLED","readReplicasSettings":{"anyAzReadReplicasCount":"2"}`), "on", "ANY_AZ:2"},
		{"removes both together, the replicas declared as ANY_AZ:0",
			settingsDeclaration(`"keyBloomFilter":"DISABLED","readReplicasSettings":{"anyAzReadReplicasCount":"0"}`), "off", "none"},
	} {
		t.Run(step.name, func(t *testing.T) {
			c := testcheck.New(t)
			before := fixture.heldPartitioning(c)
			found := fixture.compare(c, step.declared)
			c.Assert(found.Changes, qt.HasLen, 1)
			c.Assert(found.Changes[0].Kind, qt.Equals, schema.TableProperties)
			plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, step.declared))
			c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps[0].Id.Name, qt.Equals, "row-table-partitioning/000000")
			c.Assert(fixture.execute(c, "settings-"+step.name, plan).Outcome, qt.Equals, protocol.Execution_APPLIED)
			bloom, replicas := fixture.heldReadSettings(c)
			c.Assert(bloom, qt.Equals, step.bloom)
			c.Assert(replicas, qt.Equals, step.replicas)
			c.Assert(fixture.heldPartitioning(c), qt.DeepEquals, before)
			// The read-back is what the next comparison is made against: nothing is left to do.
			c.Assert(fixture.compare(c, step.declared).Changes, qt.HasLen, 0)
		})
	}
}

func TestFacetComparisonTableSettingsWithPartitioningSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, "", "")
	declared := settingsDeclaration(`"partitioningSettings":{"partitioningByLoad":"ENABLED","minPartitionsCount":"2","maxPartitionsCount":"8"},` +
		`"keyBloomFilter":"ENABLED","readReplicasSettings":{"perAzReadReplicasCount":"1"}`)
	found := fixture.compare(c, declared)
	c.Assert(found.Changes, qt.HasLen, 1)
	plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, declared))
	c.Assert(plan.Diagnostics, qt.HasLen, 0, qt.Commentf("%s", plan))
	// One call carries the three groups, as one ALTER TABLE ... SET does.
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	c.Assert(fixture.execute(c, "settings-with-partitioning", plan).Outcome, qt.Equals, protocol.Execution_APPLIED)
	held := fixture.heldPartitioning(c)
	c.Assert(held["partitioningByLoad"], qt.Equals, "ENABLED")
	c.Assert(held["minPartitionsCount"], qt.Equals, "2")
	c.Assert(held["maxPartitionsCount"], qt.Equals, "8")
	bloom, replicas := fixture.heldReadSettings(c)
	c.Assert(bloom, qt.Equals, "on")
	c.Assert(replicas, qt.Equals, "PER_AZ:1")
	c.Assert(fixture.compare(c, declared).Changes, qt.HasLen, 0)
	// Switching the partitioning by size off and on again keeps the minimum,
	// the filter and the replicas the declaration leaves out.
	for _, declared := range []string{partitioningDeclaration(`{"partitioningBySize":"DISABLED"}`), partitioningDeclaration(`{"partitioningBySize":"ENABLED"}`)} {
		fixture.apply(c, "settings-size-switch", declared, fixture.compare(c, declared).Changes)
		c.Assert(fixture.compare(c, declared).Changes, qt.HasLen, 0)
	}
	held = fixture.heldPartitioning(c)
	c.Assert(held["partitioningBySize"], qt.Equals, "ENABLED")
	c.Assert(held["partitionSizeMb"], qt.Equals, "2048")
	c.Assert(held["minPartitionsCount"], qt.Equals, "2")
	bloom, replicas = fixture.heldReadSettings(c)
	c.Assert(bloom, qt.Equals, "on")
	c.Assert(replicas, qt.Equals, "PER_AZ:1")
}

func TestFacetComparisonTableSettingsUnchangedSubprocess(t *testing.T) {
	for _, test := range []struct {
		name     string
		options  string
		declared string
	}{
		{"a declaration without the settings on a table that holds them", ` WITH (KEY_BLOOM_FILTER = ENABLED, READ_REPLICAS_SETTINGS = "PER_AZ:1")`, noTTLDeclaration},
		{"the settings the table was created with", ` WITH (KEY_BLOOM_FILTER = ENABLED, READ_REPLICAS_SETTINGS = "PER_AZ:1")`,
			settingsDeclaration(`"keyBloomFilter":"ENABLED","readReplicasSettings":{"perAzReadReplicasCount":"1"}`)},
		{"no filter and no replicas declared on a table created without them", "",
			settingsDeclaration(`"keyBloomFilter":"DISABLED","readReplicasSettings":{"perAzReadReplicasCount":"0"}`)},
		{"a filter declared off on a table created with it off", ` WITH (KEY_BLOOM_FILTER = DISABLED)`, settingsDeclaration(`"keyBloomFilter":"DISABLED"`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			fixture := newFacetFixture(c, "", test.options)
			c.Assert(fixture.compare(c, test.declared).Changes, qt.HasLen, 0)
		})
	}
}

// TestFacetComparisonTableSettingsStalePlanSubprocess plans a filter, lets
// another plan set it first, and runs the first plan: the table no longer
// holds the filter the plan captured, so the call is refused before dispatch.
func TestFacetComparisonTableSettingsStalePlanSubprocess(t *testing.T) {
	c := testcheck.New(t)
	fixture := newFacetFixture(c, "", "")
	declared := settingsDeclaration(`"keyBloomFilter":"ENABLED"`)
	stale := fixture.plan(c, fixture.compare(c, declared).Changes, desiredFacet(c, fixture.client, fixture.owner, schema.TableProperties, declared))
	c.Assert(stale.Diagnostics, qt.HasLen, 0)
	fixture.apply(c, "settings-first", declared, fixture.compare(c, declared).Changes)
	execution, err := fixture.tryExecute(c, "settings-stale", stale)
	c.Assert(err, qt.ErrorIs, provider.ErrExecution)
	c.Assert(execution.Outcome, qt.Equals, protocol.Execution_NOT_STARTED)
	bloom, _ := fixture.heldReadSettings(c)
	c.Assert(bloom, qt.Equals, "on")
}
