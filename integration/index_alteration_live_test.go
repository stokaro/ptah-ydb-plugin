//go:build integration

package integration_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/encoding/protojson"
)

// indexSpec states one declared index by its native fields.
func indexSpec(name, kind string, keys, cover string) string {
	text := `"name":"` + name + `","indexColumns":` + keys + `,"` + kind + `":{}`
	if cover != "" {
		text += `,"dataColumns":` + cover
	}
	return text
}

// declaredIndexes states the index set of records. Each declaration carries the
// requirement on the implementation table the model asks for; the planner reads
// only the specification.
func declaredIndexes(specs ...string) string {
	entries := make([]string, len(specs))
	for i, spec := range specs {
		entries[i] = `{"specification":{` + spec + `},"implementation":[{"name":"indexImplTable","settings":{"primaryKey":["payload","id"]},` +
			`"columns":[{"name":"payload","type":{"typeId":"UTF8"},"settings":{}},{"name":"id","type":{"typeId":"UINT64"},"settings":{}}]}]}`
	}
	return `{"indexes":[` + strings.Join(entries, ",") + `]}`
}

// compareIndexes compares the declared index set with the live table.
func (f *facetFixture) compareIndexes(c *qt.C, declared string) *protocol.ComparedModels {
	c.Helper()
	return f.compareRecords(c, desiredFacet(c, f.client, f.owner, schema.IndexProperties, declared))
}

// heldIndexes reads the indexes of the live table back through the provider.
func (f *facetFixture) heldIndexes(c *qt.C) map[string]*native.TableIndexDescription {
	c.Helper()
	result := map[string]*native.TableIndexDescription{}
	for _, record := range observedTable(c, f.client, f.session, f.directory).Features.Facets.Records {
		if record.Kind != schema.IndexProperties {
			continue
		}
		var observed native.IndexObservations
		c.Assert(protojson.Unmarshal(record.Value.Value.Payload, &observed), qt.IsNil)
		for _, index := range observed.Indexes {
			result[index.Description.Name] = index.Description
		}
	}
	return result
}

func TestIndexAlterationAddSubprocess(t *testing.T) {
	for _, test := range []struct {
		name, kind, field string
		keys, cover       string
		wantKeys          []string
		wantCover         []string
	}{
		{"a sync index", "globalIndex", "global_index", `["payload"]`, "", []string{"payload"}, nil},
		{"an async index", "globalAsyncIndex", "global_async_index", `["payload"]`, "", []string{"payload"}, nil},
		{"a sync index that covers a column", "globalIndex", "global_index", `["payload"]`, `["ts"]`, []string{"payload"}, []string{"ts"}},
		{"an async index on two columns", "globalAsyncIndex", "global_async_index", `["payload","ts"]`, "", []string{"payload", "ts"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := newFacetFixture(c, "", "")
			declared := declaredIndexes(indexSpec("by_payload", test.kind, test.keys, test.cover))
			found := fixture.compareIndexes(c, declared)
			c.Assert(len(found.Changes), qt.Equals, 1)
			c.Assert(found.Changes[0].Kind, qt.Equals, schema.IndexProperties)
			c.Assert(found.Changes[0].Value.Value.Kind, qt.Equals, schema.TablePropertiesChange)
			fixture.applyRecords(c, "index-add", found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, declared))
			held := fixture.heldIndexes(c)
			c.Assert(len(held), qt.Equals, 1)
			c.Assert(held["by_payload"].Status, qt.Equals, native.TableIndexDescription_STATUS_READY)
			description := held["by_payload"].ProtoReflect()
			c.Assert(string(description.WhichOneof(description.Descriptor().Oneofs().ByName("type")).Name()), qt.Equals, test.field)
			c.Assert(held["by_payload"].IndexColumns, qt.DeepEquals, test.wantKeys)
			c.Assert(held["by_payload"].DataColumns, qt.DeepEquals, test.wantCover)
			// The read-back is what the next comparison is made against: nothing is left to do.
			c.Assert(len(fixture.compareIndexes(c, declared).Changes), qt.Equals, 0)
		})
	}
}

func TestIndexAlterationDropSubprocess(t *testing.T) {
	for _, test := range []struct {
		name     string
		clauses  string
		declared string
		want     []string
	}{
		{"the only index", ", INDEX by_payload GLOBAL SYNC ON (payload)", declaredIndexes(), nil},
		{"one of two indexes", ", INDEX by_payload GLOBAL SYNC ON (payload), INDEX by_ts GLOBAL ASYNC ON (ts) COVER (payload)",
			declaredIndexes(indexSpec("by_ts", "globalAsyncIndex", `["ts"]`, `["payload"]`)), []string{"by_ts"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := newFacetFixture(c, test.clauses, "")
			found := fixture.compareIndexes(c, test.declared)
			c.Assert(len(found.Changes), qt.Equals, 1)
			fixture.applyRecords(c, "index-drop", found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, test.declared))
			var names []string
			for name := range fixture.heldIndexes(c) {
				names = append(names, name)
			}
			c.Assert(names, qt.DeepEquals, test.want)
			c.Assert(len(fixture.compareIndexes(c, test.declared).Changes), qt.Equals, 0)
		})
	}
}

func TestIndexComparisonUnchangedSubprocess(t *testing.T) {
	for _, test := range []struct {
		name     string
		clauses  string
		declared string
	}{
		{"no index on either side", "", declaredIndexes()},
		{"a sync index", ", INDEX by_payload GLOBAL SYNC ON (payload)", declaredIndexes(indexSpec("by_payload", "globalIndex", `["payload"]`, ""))},
		{"an async covering index", ", INDEX by_payload GLOBAL ASYNC ON (payload) COVER (ts)",
			declaredIndexes(indexSpec("by_payload", "globalAsyncIndex", `["payload"]`, `["ts"]`))},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := newFacetFixture(c, test.clauses, "")
			c.Assert(len(fixture.compareIndexes(c, test.declared).Changes), qt.Equals, 0)
		})
	}
}

func TestIndexAlterationRefusesAChangedIndexSubprocess(t *testing.T) {
	for _, test := range []struct {
		name     string
		declared string
		message  string
	}{
		{"another kind", declaredIndexes(indexSpec("by_payload", "globalAsyncIndex", `["payload"]`, "")),
			`holds a global index on \(payload\), but the declaration states a global async index on \(payload\)`},
		{"another key column", declaredIndexes(indexSpec("by_payload", "globalIndex", `["ts"]`, "")),
			`holds a global index on \(payload\), but the declaration states a global index on \(ts\)`},
		{"a covered column", declaredIndexes(indexSpec("by_payload", "globalIndex", `["payload"]`, `["ts"]`)),
			`but the declaration states a global index on \(payload\) covering \(ts\)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			fixture := newFacetFixture(c, ", INDEX by_payload GLOBAL SYNC ON (payload)", "")
			found := fixture.compareIndexes(c, test.declared)
			c.Assert(len(found.Changes), qt.Equals, 1)
			plan := fixture.plan(c, found.Changes, desiredFacet(c, fixture.client, fixture.owner, schema.IndexProperties, test.declared))
			c.Assert(len(plan.Diagnostics), qt.Equals, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, `(?s).*`+test.message+`.*YDB cannot alter an index in place.*`)
			c.Assert(len(plan.Contributions), qt.Equals, 0)
			// The refusal leaves the index as it was.
			held := fixture.heldIndexes(c)
			c.Assert(len(held), qt.Equals, 1)
			c.Assert(held["by_payload"].GetGlobalIndex(), qt.IsNotNil)
			c.Assert(held["by_payload"].IndexColumns, qt.DeepEquals, []string{"payload"})
		})
	}
}
