package tablecreate_test

import (
	"encoding/json"
	"maps"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// cacheModes is a session that takes a column family's cache mode.
var cacheModes = map[string]bool{tablecreate.CacheModeCapability: true}

// cacheModeRefusal is the refusal of a declared cache mode on a target
// without the capability, in the host's words with the flag YDB keeps it
// behind.
const cacheModeRefusal = `table "app/records" declares column family cache mode, which requires target capability column_family_cache_mode, ` +
	`unavailable on this ydb target; YDB keeps it behind feature flag EnableTableCacheModes: turn the flag on, and name the cluster's ` +
	`monitoring endpoint in the URL \(monitoring=http://host:8765\) so Ptah reads the flags before it plans`

// renderedText renders a plan's steps in the TEXT form, which reads each
// operation back from its payload.
func renderedText(c *qt.C, plan *protocol.PlannedFeatures, capabilities map[string]bool) []string {
	c.Helper()
	rendering := testplanning.RenderRequest(backend.Description(), plan, capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	var statements []string
	for _, unit := range rendered.Units {
		statements = append(statements, unit.GetSql())
	}
	return statements
}

// payloadRequest is the request an alteration's payload carries, as JSON.
func payloadRequest(c *qt.C, value *protocol.Envelope) string {
	c.Helper()
	var payload struct {
		Request json.RawMessage `json:"request"`
	}
	c.Assert(json.Unmarshal(value.Payload, &payload), qt.IsNil)
	return string(payload.Request)
}

// A declared cache mode is set on a family the table holds when it differs
// from the one held, and given to a family the plan adds. The operation's
// payload carries it as the upstream protocol spells it in JSON, and the TEXT
// form writes CACHE_MODE after the settings the host's embedded engine writes
// before it.
func TestAlterPlan_CacheMode_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, desired, held string
		wantRequest         string
		wantText            string
	}{
		{name: "a cache mode on a family that holds none",
			desired: withFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}`), held: heldFamilies(`{"name":"archive"}`),
			wantRequest: `{"alterColumnFamilies":[{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}]}`,
			wantText:    "ALTER TABLE `app/records` ALTER FAMILY `archive` SET CACHE_MODE 'in_memory';"},
		{name: "another cache mode than the one held",
			desired: withFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_REGULAR"}`), held: heldFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}`),
			wantRequest: `{"alterColumnFamilies":[{"name":"archive","cacheMode":"CACHE_MODE_REGULAR"}]}`,
			wantText:    "ALTER TABLE `app/records` ALTER FAMILY `archive` SET CACHE_MODE 'regular';"},
		{name: "a cache mode beside a compression",
			desired:     withFamilies(`{"name":"archive","compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_REGULAR"}`),
			held:        heldFamilies(`{"name":"archive","compression":"COMPRESSION_NONE","cacheMode":"CACHE_MODE_IN_MEMORY"}`),
			wantRequest: `{"alterColumnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_REGULAR"}]}`,
			wantText:    "ALTER TABLE `app/records` ALTER FAMILY `archive` SET COMPRESSION 'lz4', ALTER FAMILY `archive` SET CACHE_MODE 'regular';"},
		{name: "a family added with a cache mode",
			desired: withFamilies(`{"name":"archive","compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_IN_MEMORY"}`), held: plainTable,
			wantRequest: `{"addColumnFamilies":[{"name":"archive","compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_IN_MEMORY"}]}`,
			wantText:    "ALTER TABLE `app/records` ADD FAMILY `archive` (COMPRESSION = 'lz4', CACHE_MODE = 'in_memory');"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := alterRequest(c, alterCase{desiredTable: test.desired, heldTable: test.held, capabilities: cacheModes})
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 1)
			c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
			c.Assert(payloadRequest(c, plan.Contributions[0].Steps[0].Operation.Value.Value), qt.JSONEquals, jsonValue(c, test.wantRequest))
			c.Assert(renderedText(c, plan, request.Capabilities), qt.DeepEquals, []string{test.wantText})
		})
	}
}

// withFamilies is a declaration of app.records stating families.
func withFamilies(families string) string {
	return `{"primaryKey":["id"],"columnFamilies":[` + families + `]}`
}

// heldFamilies is a read of app.records holding the default family and
// families.
func heldFamilies(families string) string {
	return `{"primaryKey":["id"],"columnFamilies":[{"name":"default"},` + families + `]}`
}

// A cache mode the family holds is not set again, and one the declaration
// leaves out stays as the table holds it, also on a target whose flag no
// longer takes a cache mode.
func TestAlterPlan_CacheMode_NoChange(t *testing.T) {
	for _, test := range []struct {
		name, desired, held string
		capabilities        map[string]bool
	}{
		{name: "the cache mode the family holds", desired: withFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}`),
			held: heldFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}`), capabilities: cacheModes},
		{name: "a cache mode the declaration leaves out", desired: withFamilies(`{"name":"archive","compression":"COMPRESSION_LZ4"}`),
			held: heldFamilies(`{"name":"archive","compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_IN_MEMORY"}`), capabilities: cacheModes},
		{name: "a cache mode the declaration leaves out on a target that takes none", desired: withFamilies(`{"name":"archive"}`),
			held: heldFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, alterCase{desiredTable: test.desired, heldTable: test.held, capabilities: test.capabilities}))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// A declared cache mode on a target without the capability is refused before
// anything is planned, regular included and whatever the table holds: the
// table service of a line that does not know the field drops it and alters
// the table without it.
func TestAlterPlan_CacheMode_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, desired, held string }{
		{name: "a cache mode to set", desired: withFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}`), held: heldFamilies(`{"name":"archive"}`)},
		{name: "a family added with a cache mode", desired: withFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_REGULAR"}`), held: plainTable},
		{name: "the cache mode the family holds", desired: withFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}`),
			held: heldFamilies(`{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			plan, err := backend.New().PlanFeatures(c.Context(), alterRequest(c, alterCase{desiredTable: test.desired, heldTable: test.held}))
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Matches, cacheModeRefusal)
			c.Assert(plan.Contributions, qt.HasLen, 0)
		})
	}
}

// cacheModeTable is app.records declaring family hot, which holds the column
// value, with the family settings given.
func cacheModeTable(c *qt.C, family string) *protocol.DeclaredTable {
	c.Helper()
	table := declaration()
	table.Features = &protocol.CapturedFeatures{Facets: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Records: []*protocol.ComparisonRecord{
		facet(c, schema.TableProperties, "", `{"primaryKey":["second","first"],"columnFamilies":[`+family+`]}`),
		facet(c, schema.ColumnProperties, "value", `{"family":"hot"}`),
	}}}
	return table
}

// A new table is created with the cache mode each family declares: the
// creation's payload carries it as the upstream protocol spells it in JSON,
// the request read back from the payload holds it, and the TEXT form writes
// it as the family's CACHE_MODE.
func TestCreation_CacheMode_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, family, wantFamilies, wantEntry string
	}{
		{name: "in memory", family: `{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"}`,
			wantFamilies: `[{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"}]`, wantEntry: "FAMILY `hot` (CACHE_MODE = 'in_memory')"},
		{name: "regular beside the other settings", family: `{"name":"hot","data":{"media":"hdd"},"compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_REGULAR"}`,
			wantFamilies: `[{"name":"hot","data":{"media":"hdd"},"compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_REGULAR"}]`,
			wantEntry:    "FAMILY `hot` (DATA = 'hdd', COMPRESSION = 'lz4', CACHE_MODE = 'regular')"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			captured, err := tablecreate.Capture("create", owner(), cacheModeTable(c, test.family), cacheModes)
			c.Assert(err, qt.IsNil)
			var payload struct {
				ColumnFamilies json.RawMessage `json:"columnFamilies"`
			}
			c.Assert(json.Unmarshal(captured.Value.Payload, &payload), qt.IsNil)
			c.Assert(string(payload.ColumnFamilies), qt.JSONEquals, jsonValue(c, test.wantFamilies))
			request, err := tablecreate.Request(captured.Value)
			c.Assert(err, qt.IsNil)
			statement, err := tablecreate.RowTableStatement("app/records", request)
			c.Assert(err, qt.IsNil)
			c.Assert(statement, qt.Equals, "CREATE TABLE `app/records` (\n    `first` Uint64 NOT NULL,\n    `second` Utf8 NOT NULL,\n    `value` Decimal(22,9) FAMILY `hot`,\n"+
				"    PRIMARY KEY (`second`, `first`),\n    "+test.wantEntry+"\n);")
		})
	}
}

// A new table that declares a cache mode is refused on a target without the
// capability, regular included.
func TestCreation_CacheMode_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, family string }{
		{name: "in memory", family: `{"name":"hot","cacheMode":"CACHE_MODE_IN_MEMORY"}`},
		{name: "regular", family: `{"name":"hot","cacheMode":"CACHE_MODE_REGULAR"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			captured, err := tablecreate.Capture("create", owner(), cacheModeTable(c, test.family), nil)
			c.Assert(err, qt.ErrorMatches, cacheModeRefusal)
			c.Assert(captured, qt.IsNil)
		})
	}
}

// A creation payload whose family states a cache mode the upstream protocol
// does not name, or a field the provider's family does not declare, is
// refused.
func TestCreation_CacheMode_RefusesAnUnknownSetting(t *testing.T) {
	for _, test := range []struct{ name, family string }{
		{name: "a cache mode the protocol does not name", family: `{"name":"hot","cacheMode":"CACHE_MODE_SWAP"}`},
		{name: "a compression level", family: `{"name":"hot","compressionLevel":3}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			captured, err := tablecreate.Capture("create", owner(), cacheModeTable(c, `{"name":"hot"}`), cacheModes)
			c.Assert(err, qt.IsNil)
			var payload map[string]json.RawMessage
			c.Assert(json.Unmarshal(captured.Value.Payload, &payload), qt.IsNil)
			payload["columnFamilies"] = json.RawMessage(`[` + test.family + `]`)
			forged, err := json.Marshal(payload)
			c.Assert(err, qt.IsNil)
			captured.Value.Payload = forged

			request, err := tablecreate.Request(captured.Value)

			c.Assert(err, qt.ErrorMatches, `invalid YDB table creation JSON`)
			c.Assert(request, qt.IsNil)
		})
	}
}

// cacheModeRebuild is rebuildRequest on a table whose family cold holds
// payload in memory, with the declaration's table properties given, none for
// empty, on a target with capabilities.
func cacheModeRebuild(c *qt.C, desired string, capabilities map[string]bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := rebuildRequest(c, &protocol.PlanningPolicy{AllowTableRebuild: true, TableRebuildRequest: "--allow-table-rebuild"})
	fixture := alterRequest(c, alterCase{desiredTable: desired, heldTable: heldFamilies(`{"name":"cold","compression":"COMPRESSION_LZ4","cacheMode":"CACHE_MODE_IN_MEMORY"}`),
		heldColumns: map[string]string{"payload": `{"family":"cold"}`}, held: []string{"payload"}})
	request.Tables[0].Current.Features = fixture.Tables[0].Current.Features
	if desired != "" {
		request.Tables[0].Desired.Features = fixture.Tables[0].Desired.Features
	}
	maps.Copy(request.Capabilities, capabilities)
	return request
}

// The new table of a rebuild keeps the cache mode a family of the old table
// holds where the declaration states none, and takes the one it states.
func TestRebuildPlan_CacheMode_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, desired, wantEntry string }{
		{name: "a cache mode the declaration leaves out", wantEntry: "FAMILY `cold` (COMPRESSION = 'lz4', CACHE_MODE = 'in_memory')"},
		{name: "a cache mode a declared family leaves out", desired: withFamilies(`{"name":"cold","compression":"COMPRESSION_NONE"}`),
			wantEntry: "FAMILY `cold` (COMPRESSION = 'off', CACHE_MODE = 'in_memory')"},
		{name: "a cache mode the declaration states", desired: withFamilies(`{"name":"cold","cacheMode":"CACHE_MODE_REGULAR"}`),
			wantEntry: "FAMILY `cold` (COMPRESSION = 'lz4', CACHE_MODE = 'regular')"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := cacheModeRebuild(c, test.desired, cacheModes)
			plan, err := backend.New().PlanFeatures(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(plan.Diagnostics, qt.HasLen, 0)
			c.Assert(renderedText(c, plan, request.Capabilities)[0], qt.Equals, "CREATE TABLE `app/__ptah_rebuild_records` (\n    `id` Uint64 NOT NULL,\n"+
				"    `payload` Utf8 FAMILY `cold`,\n    `n` Int64,\n    PRIMARY KEY (`id`),\n    "+test.wantEntry+"\n);")
		})
	}
}

// A rebuild that would write a cache mode the old table holds is refused on
// a target without the capability, whose table service would create the new
// table without it.
func TestRebuildPlan_CacheMode_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	plan, err := backend.New().PlanFeatures(c.Context(), cacheModeRebuild(c, "", nil))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `rebuilding table "app/records" with column family cache mode, which requires target capability column_family_cache_mode, `+
		`unavailable on this ydb target; YDB keeps it behind feature flag EnableTableCacheModes: .*`)
	c.Assert(plan.Contributions, qt.HasLen, 0)
}
