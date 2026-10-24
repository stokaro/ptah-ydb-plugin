package tablecompare_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// storageSide states the facets of one side of app.records for a storage
// comparison: its column storage, its row-table properties, and what its
// source knows of column storage. An empty payload states none.
type storageSide struct {
	storage string
	table   string
	// known makes the side's knowledge of column storage complete, as a YAML
	// or Go source states it.
	known bool
}

func storageState(c *qt.C, representation protocol.ValueInput_Representation, facets storageSide) *protocol.ComparisonState {
	c.Helper()
	result := &protocol.ComparisonState{Representation: representation, Coverage: &protocol.ComparisonCoverage{}}
	knowledge := protocol.ModelKnowledge_UNINSPECTED
	if facets.known {
		knowledge = protocol.ModelKnowledge_COMPLETE
	}
	for _, candidate := range schema.Models() {
		if candidate.Kind == schema.ColumnStorageProperties && candidate.Representation == representation {
			result.Coverage.Kinds = append(result.Coverage.Kinds, &protocol.ComparisonKindCoverage{Model: candidate,
				Sources: []*protocol.ComparisonKnowledgeSource{{Outside: testcomparison.Knowledge(knowledge)}}})
		}
	}
	if facets.storage != "" {
		result.Records = append(result.Records, record(c, representation, schema.ColumnStorageProperties, "", facets.storage))
	}
	if facets.table != "" {
		result.Records = append(result.Records, record(c, representation, schema.TableProperties, "", facets.table))
	}
	return result
}

func storageRequest(c *qt.C, desired, current storageSide) *protocol.CompareModelsRequest {
	c.Helper()
	return &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: tablecompare.ServiceID, Kinds: tablecompare.Kinds(),
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		Desired:     storageState(c, protocol.ValueInput_DESIRED, desired), Current: storageState(c, protocol.ValueInput_OBSERVED, current),
		Parents: []*protocol.ComparisonParent{{Subject: subject("table", ""), Desired: true, Current: true}}}
}

const (
	heldColumnStorage = `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64","ttl":{"enabled":{"column":"ts","expireAfterSeconds":"86400","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`
	dailyColumnTTL    = `{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}`
	heldRowTable      = `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}]}`
)

// TestCompareStorage_NoChange reports nothing for a column table that already
// holds what the declaration states, or that a source cannot describe.
func TestCompareStorage_NoChange(t *testing.T) {
	for _, test := range []struct {
		name             string
		desired, current storageSide
	}{
		{"the same storage", storageSide{storage: `{"hashColumns":["id"],"partitions":"3","ttl":` + dailyColumnTTL + `}`}, storageSide{storage: heldColumnStorage}},
		{"the server's choices", storageSide{storage: `{"ttl":` + dailyColumnTTL + `}`}, storageSide{storage: heldColumnStorage}},
		{"a source that cannot describe the storage", storageSide{}, storageSide{storage: heldColumnStorage}},
		{"local indexes the declaration leaves out", storageSide{storage: `{"ttl":` + dailyColumnTTL + `}`},
			storageSide{storage: `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64","ttl":` + dailyColumnTTL + `,` +
				`"localIndexes":[{"name":"mm","columns":["ts"],"storageId":"__LOCAL_METADATA","minMax":{}}]}`}},
		{"two row tables", storageSide{table: heldRowTable, known: true}, storageSide{table: heldRowTable}},
		{"a disabled TTL is none", storageSide{storage: `{"ttl":{"disabled":{}}}`}, storageSide{storage: `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64"}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result := compare(c, storageRequest(c, test.desired, test.current))
			c.Assert(result.Changes, qt.HasLen, 0)
		})
	}
}

// TestCompareStorage_ReportsAChange reports a change with both operands, a
// missing one for row storage on that side.
func TestCompareStorage_ReportsAChange(t *testing.T) {
	for _, test := range []struct {
		name             string
		desired, current storageSide
		before, after    bool
	}{
		{"another shard count", storageSide{storage: `{"partitions":"4","ttl":` + dailyColumnTTL + `}`}, storageSide{storage: heldColumnStorage}, true, true},
		{"another epoch unit", storageSide{storage: `{"ttl":{"enabled":{"column":"ts","columnUnit":"2","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`},
			storageSide{storage: `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64","ttl":{"enabled":{"column":"ts","columnUnit":"1","expireAfterSeconds":"86400","tiers":[{"applyAfterSeconds":"86400","delete":{}}]}}}`}, true, true},
		{"another hash key", storageSide{storage: `{"hashColumns":["ts","id"],"ttl":` + dailyColumnTTL + `}`}, storageSide{storage: heldColumnStorage}, true, true},
		{"another TTL", storageSide{storage: `{"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"3600","delete":{}}]}}}`}, storageSide{storage: heldColumnStorage}, true, true},
		{"no TTL", storageSide{storage: `{}`}, storageSide{storage: heldColumnStorage}, true, true},
		{"a column table over a row table", storageSide{storage: `{}`}, storageSide{table: heldRowTable}, false, true},
		{"a row table a complete source declares over a column table", storageSide{known: true}, storageSide{storage: heldColumnStorage}, true, false},
		{"row-table settings over a column table", storageSide{table: `{"keyBloomFilter":"ENABLED"}`}, storageSide{storage: heldColumnStorage}, true, false},
		{"local indexes the declaration states otherwise", storageSide{storage: `{"ttl":` + dailyColumnTTL + `,"localIndexes":[{"name":"mm","columns":["ts"],"storageId":"__LOCAL_METADATA","minMax":{}}]}`},
			storageSide{storage: heldColumnStorage}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result := compare(c, storageRequest(c, test.desired, test.current))
			c.Assert(result.Changes, qt.HasLen, 1)
			change := result.Changes[0]
			c.Assert(change.Kind, qt.Equals, schema.ColumnStorageProperties)
			c.Assert(change.Value.Value.Kind, qt.Equals, schema.ColumnStorageChange)
			var operands map[string]json.RawMessage
			c.Assert(json.Unmarshal(change.Value.Value.Payload, &operands), qt.IsNil)
			_, before := operands["before"]
			_, after := operands["after"]
			c.Assert(before, qt.Equals, test.before)
			c.Assert(after, qt.Equals, test.after)
			c.Assert(change.Value.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
		})
	}
}
