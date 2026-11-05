package schemavalidation_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// cachedTable is app.docs declaring family hot with the cache mode given.
func cachedTable(c *qt.C, mode string) *protocol.ValidationTable {
	c.Helper()
	declared := keyed("app", "docs")
	model := schema.TableDesiredModel()
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "docs", Representation: protocol.ValueInput_DESIRED,
		Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256,
			Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "docs"}, Payload: []byte(`{"columnFamilies":[{"name":"hot","cacheMode":"` + mode + `"}]}`)}}}})
	c.Assert(err, qt.IsNil)
	declared.Declaration.Features.Facets.Records = []*protocol.ComparisonRecord{{Kind: model.Kind, Subject: subject("app", "docs"), Value: values.Values[0]}}
	return declared
}

// A declared cache mode validates on a target that takes one.
func TestValidateSchema_CacheMode_HappyPath(t *testing.T) {
	for _, mode := range []string{"CACHE_MODE_REGULAR", "CACHE_MODE_IN_MEMORY"} {
		t.Run(mode, func(t *testing.T) {
			c := testcheck.New(t)
			request := newRequest(cachedTable(c, mode))
			request.Capabilities[tablecreate.CacheModeCapability] = true
			c.Assert(diagnostics(c, request), qt.HasLen, 0)
		})
	}
}

// A declared cache mode, regular included, is unsupported on a target
// without the capability, as the host's bundled validation reports it for
// --server-version 25.1 and 25.3.
func TestValidateSchema_CacheMode_FailurePath(t *testing.T) {
	for _, mode := range []string{"CACHE_MODE_REGULAR", "CACHE_MODE_IN_MEMORY"} {
		t.Run(mode, func(t *testing.T) {
			c := testcheck.New(t)
			got := diagnostics(c, newRequest(cachedTable(c, mode)))
			c.Assert(got, qt.HasLen, 1)
			c.Assert(got[0].Code, qt.Equals, protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE)
			c.Assert(got[0].Message, qt.Matches, `table "app/docs" declares column family cache mode, which requires target capability column_family_cache_mode, `+
				`unavailable on this ydb target; YDB keeps it behind feature flag EnableTableCacheModes: .*`)
		})
	}
}
