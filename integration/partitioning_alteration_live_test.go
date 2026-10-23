//go:build integration

package integration_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// heldPartitioning is the partitioning settings the provider reads back, as
// JSON, or "null" when the table reports none.
func (f *facetFixture) heldPartitioning(c *qt.C) map[string]any {
	c.Helper()
	var properties struct {
		Partitioning map[string]any `json:"partitioningSettings"`
	}
	c.Assert(json.Unmarshal(f.heldTable(c), &properties), qt.IsNil)
	return properties.Partitioning
}

func partitioningDeclaration(settings string) string {
	return `{"primaryKey":["id"],"partitioningSettings":` + settings + `}`
}

func TestFacetComparisonPartitioningSubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newFacetFixture(c, "", "")
	// Each step starts from what the previous one left in the table, and
	// declares one setting. unchanged lists the settings the step must leave
	// as they were; the server derives the others from the one that changes.
	for _, step := range []struct {
		name      string
		declared  string
		setting   string
		want      any
		unchanged []string
	}{
		{"switches partitioning by load on", partitioningDeclaration(`{"partitioningByLoad":"ENABLED"}`), "partitioningByLoad", "ENABLED",
			[]string{"partitioningBySize", "partitionSizeMb", "minPartitionsCount", "maxPartitionsCount"}},
		{"changes the partition size", partitioningDeclaration(`{"partitionSizeMb":"512"}`), "partitionSizeMb", "512",
			[]string{"partitioningBySize", "partitioningByLoad", "minPartitionsCount", "maxPartitionsCount"}},
		{"raises the minimum partition count", partitioningDeclaration(`{"minPartitionsCount":"2"}`), "minPartitionsCount", "2",
			[]string{"partitioningBySize", "partitioningByLoad", "partitionSizeMb", "maxPartitionsCount"}},
		{"lowers the maximum partition count", partitioningDeclaration(`{"maxPartitionsCount":"8"}`), "maxPartitionsCount", "8",
			[]string{"partitioningBySize", "partitioningByLoad", "partitionSizeMb", "minPartitionsCount"}},
		{"switches partitioning by size off", partitioningDeclaration(`{"partitioningBySize":"DISABLED"}`), "partitioningBySize", "DISABLED",
			[]string{"partitioningByLoad", "minPartitionsCount", "maxPartitionsCount"}},
		{"switches partitioning by size on and keeps the minimum it states",
			partitioningDeclaration(`{"partitioningBySize":"ENABLED","minPartitionsCount":"2"}`), "partitioningBySize", "ENABLED",
			[]string{"partitioningByLoad", "minPartitionsCount", "maxPartitionsCount"}},
	} {
		t.Run(step.name, func(t *testing.T) {
			c := qt.New(t)
			before := fixture.heldPartitioning(c)
			found := fixture.compare(c, step.declared)
			c.Assert(found.Changes, qt.HasLen, 1)
			c.Assert(found.Changes[0].Kind, qt.Equals, schema.TableProperties)
			c.Assert(found.Changes[0].Value.Value.Kind, qt.Equals, schema.TablePropertiesChange)
			fixture.apply(c, "partitioning-"+step.name, step.declared, found.Changes)
			after := fixture.heldPartitioning(c)
			c.Assert(after[step.setting], qt.Equals, step.want)
			for _, name := range step.unchanged {
				c.Assert(after[name], qt.DeepEquals, before[name], qt.Commentf("%s", name))
			}
			c.Assert(after["partitionBy"], qt.DeepEquals, before["partitionBy"])
			// The read-back is what the next comparison is made against: nothing is left to do.
			c.Assert(fixture.compare(c, step.declared).Changes, qt.HasLen, 0)
		})
	}
}

func TestFacetComparisonPartitioningSettingsTogetherSubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newFacetFixture(c, "", "")
	declared := partitioningDeclaration(`{"partitioningByLoad":"ENABLED","partitionSizeMb":"512","minPartitionsCount":"2","maxPartitionsCount":"8"}`)
	found := fixture.compare(c, declared)
	c.Assert(found.Changes, qt.HasLen, 1)
	fixture.apply(c, "partitioning-together", declared, found.Changes)
	after := fixture.heldPartitioning(c)
	c.Assert(after["partitioningByLoad"], qt.Equals, "ENABLED")
	c.Assert(after["partitionSizeMb"], qt.Equals, "512")
	c.Assert(after["minPartitionsCount"], qt.Equals, "2")
	c.Assert(after["maxPartitionsCount"], qt.Equals, "8")
	c.Assert(fixture.compare(c, declared).Changes, qt.HasLen, 0)
}

func TestFacetComparisonPartitioningDefaultsSubprocess(t *testing.T) {
	for _, test := range []struct {
		name     string
		options  string
		declared string
	}{
		{"a declaration without partitioning settings", "", noTTLDeclaration},
		{"the default switch stated", "", partitioningDeclaration(`{"partitioningBySize":"ENABLED"}`)},
		{"settings the table was created with",
			` WITH (AUTO_PARTITIONING_BY_LOAD = ENABLED, AUTO_PARTITIONING_PARTITION_SIZE_MB = 512, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 8)`,
			partitioningDeclaration(`{"partitioningByLoad":"ENABLED","partitionSizeMb":"512","minPartitionsCount":"2","maxPartitionsCount":"8"}`)},
		{"the partition columns the table was created with", "", partitioningDeclaration(`{"partitionBy":["id"]}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := newFacetFixture(c, "", test.options)
			c.Assert(fixture.compare(c, test.declared).Changes, qt.HasLen, 0)
		})
	}
}
