//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// On a line that takes a cache mode, a family's cache mode is set and then
// changed on twin tables, once through the native call and once through the
// YQL statement the TEXT form writes for a migration file. Both read back the
// same, and after either nothing is left to plan.
func TestTextUnitsChangeACacheModeAsTheNativeCallSubprocess(t *testing.T) {
	c := testcheck.New(t)
	const clauses = ", FAMILY archive (COMPRESSION = 'off')"
	native := newFacetFixture(c, clauses, "")
	requireSessionCapability(c, native.session, tablecreate.CacheModeCapability, true)
	text := newFacetFixtureListing(c, clauses, "", provider.TextUnitsFeature)
	text.form = protocol.RenderRequest_TEXT
	apply := func(f *facetFixture) any {
		var held []any
		for _, declared := range []string{`{"primaryKey":["id"],"columnFamilies":[{"name":"archive","cacheMode":"CACHE_MODE_IN_MEMORY"}]}`,
			`{"primaryKey":["id"],"columnFamilies":[{"name":"archive","cacheMode":"CACHE_MODE_REGULAR"}]}`} {
			f.apply(c, "family", declared, f.compare(c, declared).Changes)
			c.Assert(f.compare(c, declared).Changes, qt.HasLen, 0)
			held = append(held, f.heldSettings(c)["columnFamilies"])
		}
		return held
	}

	held := apply(native)
	c.Assert(apply(text), qt.DeepEquals, held)
	c.Assert(held, qt.DeepEquals, []any{
		[]any{map[string]any{"name": "default", "compression": "COMPRESSION_NONE"}, map[string]any{"name": "archive", "compression": "COMPRESSION_NONE", "cacheMode": "CACHE_MODE_IN_MEMORY"}},
		[]any{map[string]any{"name": "default", "compression": "COMPRESSION_NONE"}, map[string]any{"name": "archive", "compression": "COMPRESSION_NONE", "cacheMode": "CACHE_MODE_REGULAR"}},
	})
	var statements []string
	for _, unit := range text.rendered {
		statements = append(statements, unit.GetSql())
	}
	c.Assert(statements, qt.DeepEquals, []string{"ALTER TABLE `" + text.directory + "/records` ALTER FAMILY `archive` SET CACHE_MODE 'in_memory';",
		"ALTER TABLE `" + text.directory + "/records` ALTER FAMILY `archive` SET CACHE_MODE 'regular';"})
}
