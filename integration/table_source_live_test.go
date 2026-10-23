//go:build integration

package integration_test

import (
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
