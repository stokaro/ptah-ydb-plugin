//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// decodedTableFacet decodes a captured source in the provider subprocess and
// returns the one table facet it declares, bound to the fixture's live table
// the way the host binds a source facet before it compares.
func (f *facetFixture) decodedTableFacet(c *qt.C, request *protocol.DecodeSourceRequest) *protocol.ComparisonRecord {
	c.Helper()
	result, err := f.client.DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(f.client.Description(), request, result), qt.IsNil)
	var payloads []string
	for _, fragment := range result.Documents[0].Fragments {
		for _, value := range fragment.Values {
			c.Assert(value.Value.Value.Kind, qt.Equals, schema.TableProperties)
			payloads = append(payloads, string(value.Value.Value.Payload))
		}
	}
	c.Assert(payloads, qt.HasLen, 1)
	return desiredFacet(c, f.client, f.owner, schema.TableProperties, payloads[0])
}

// The facet a YAML source declares is the one the comparison and the
// alteration planner read: it is compared with the live table, planned,
// executed, and read back.
func TestTableSourceYAMLReachesAlterationSubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newFacetFixture(c, ", FAMILY archive (COMPRESSION = 'off')", "")
	source := fmt.Sprintf(`{"tables":{"records":{"schema":%q,"auto_partitioning_by_load":"ENABLED","auto_partitioning_min_partitions_count":2,`+
		`"column_families":{"archive":{"compression":"LZ4"}}}}}`, fixture.directory)
	request, err := testsource.YAMLTables(source)
	c.Assert(err, qt.IsNil)
	declared := fixture.decodedTableFacet(c, request)
	found := fixture.compareRecords(c, declared)
	c.Assert(found.Changes, qt.HasLen, 1)
	c.Assert(found.Changes[0].Kind, qt.Equals, schema.TableProperties)
	fixture.applyRecords(c, "source-yaml", found.Changes, declared)
	held := string(fixture.heldTable(c))
	c.Assert(held, qt.Contains, `"partitioningByLoad":"ENABLED"`)
	c.Assert(held, qt.Contains, `"minPartitionsCount":"2"`)
	c.Assert(held, qt.Contains, "COMPRESSION_LZ4")
	// The read-back is what the next comparison is made against: nothing is left to do.
	c.Assert(fixture.compareRecords(c, declared).Changes, qt.HasLen, 0)
}

// The same for a Go directive, which names the table of its struct.
func TestTableSourceGoDirectiveReachesAlterationSubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newFacetFixture(c, ", FAMILY archive (COMPRESSION = 'lz4')", "")
	declaration := testsource.GoDeclaration{Directive: "ptah:schema:columnfamily", Struct: "Record", File: "records.go", FileSize: 1000,
		Attributes: map[string]string{"name": "archive", "compression": "off"},
		Start:      testsource.GoPosition{Offset: 100, Line: 3, Column: 1}, End: testsource.GoPosition{Offset: 160, Line: 3, Column: 61}}
	request, err := testsource.GoDirectives([]testsource.GoTable{{Schema: fixture.directory, Name: "records", Struct: "Record"}}, []testsource.GoDeclaration{declaration})
	c.Assert(err, qt.IsNil)
	declared := fixture.decodedTableFacet(c, request)
	found := fixture.compareRecords(c, declared)
	c.Assert(found.Changes, qt.HasLen, 1)
	fixture.applyRecords(c, "source-go", found.Changes, declared)
	c.Assert(string(fixture.heldTable(c)), qt.Contains, "COMPRESSION_NONE")
	c.Assert(fixture.compareRecords(c, declared).Changes, qt.HasLen, 0)
}

// tableDirective is a ptah:schema:table comment of the fixture's struct with
// the attributes the provider claimed, as the host sends them.
func tableDirective(attributes map[string]string) testsource.GoDeclaration {
	return testsource.GoDeclaration{Directive: "ptah:schema:table", Struct: "Record", File: "records.go", FileSize: 1000, Attributes: attributes,
		Start: testsource.GoPosition{Offset: 100, Line: 3, Column: 1}, End: testsource.GoPosition{Offset: 190, Line: 3, Column: 91}}
}

func (f *facetFixture) goTable(c *qt.C, attributes map[string]string) *protocol.ComparisonRecord {
	c.Helper()
	request, err := testsource.GoDirectives([]testsource.GoTable{{Schema: f.directory, Name: "records", Struct: "Record"}}, []testsource.GoDeclaration{tableDirective(attributes)})
	c.Assert(err, qt.IsNil)
	return f.decodedTableFacet(c, request)
}

// The attributes of ptah:schema:table reach the live table the way the
// directive's attributes of the bundled source do: the partitioning is set, and
// so is a TTL, which the table keeps while the source states it.
func TestTableSourceGoTableAttributesReachAlterationSubprocess(t *testing.T) {
	c := qt.New(t)
	fixture := newFacetFixture(c, "", ` WITH (TTL = Interval("PT1H") ON ts)`)
	ttl := func(interval string) map[string]string {
		return map[string]string{"platform.ydb.row_deletion_column": "ts", "platform.ydb.row_deletion_interval": interval}
	}
	steps := []struct {
		name       string
		attributes map[string]string
		wantTTL    string
		wantHeld   string
	}{
		{"partitioning while the source states the TTL the table holds", map[string]string{"platform.ydb.row_deletion_column": "ts", "platform.ydb.row_deletion_interval": "PT1H",
			"auto_partitioning_by_load": "ENABLED", "auto_partitioning_min_partitions_count": "2"}, hourlyTTL, `"partitioningByLoad":"ENABLED"`},
		{"a new interval", ttl("P1D"), dailyTTL, ""},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			c := qt.New(t)
			declared := fixture.goTable(c, step.attributes)
			found := fixture.compareRecords(c, declared)
			c.Assert(found.Changes, qt.HasLen, 1)
			fixture.applyRecords(c, "go-table-"+step.name, found.Changes, declared)
			c.Assert(fixture.heldTTL(c), qt.JSONEquals, json.RawMessage(step.wantTTL))
			c.Assert(string(fixture.heldTable(c)), qt.Contains, step.wantHeld)
			c.Assert(fixture.compareRecords(c, declared).Changes, qt.HasLen, 0)
		})
	}
}

// A source that states table settings and no TTL removes the TTL the table
// holds, and a table the source states nothing for keeps it. The bundled
// source does the same: its Go and YAML sources claim the TTL of every table
// they describe, so `ptah schema apply` over a file without a TTL plans
// `ALTER TABLE ... RESET (TTL)`, measured with the host built from
// 7076368bcb2093fd0d074765dd5c597dccdc1491 on 25.1.4.7 and 26.2.1.14. The
// provider removes it for a table that has a facet, the one it can see, and
// leaves the TTL of a table that has none.
func TestTableSourceStatingNoTTLRemovesTheTTLSubprocess(t *testing.T) {
	t.Run("a table with a facet", func(t *testing.T) {
		c := qt.New(t)
		fixture := newFacetFixture(c, "", ` WITH (TTL = Interval("PT1H") ON ts)`)
		declared := fixture.goTable(c, map[string]string{"auto_partitioning_by_load": "ENABLED"})
		found := fixture.compareRecords(c, declared)
		c.Assert(found.Changes, qt.HasLen, 1)
		fixture.applyRecords(c, "go-table-without-ttl", found.Changes, declared)
		c.Assert(fixture.heldTTL(c), qt.Equals, "null")
		c.Assert(string(fixture.heldTable(c)), qt.Contains, `"partitioningByLoad":"ENABLED"`)
	})
	t.Run("a table without one", func(t *testing.T) {
		c := qt.New(t)
		fixture := newFacetFixture(c, "", ` WITH (TTL = Interval("PT1H") ON ts)`)
		c.Assert(fixture.compareRecords(c).Changes, qt.HasLen, 0)
		c.Assert(fixture.heldTTL(c), qt.JSONEquals, json.RawMessage(hourlyTTL))
	})
}
