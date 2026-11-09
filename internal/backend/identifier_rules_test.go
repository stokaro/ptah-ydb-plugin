package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
)

// YDB compares table, column and index names exactly, keeps an index name
// per table, and has no default schema: the engine states no folding, and a
// directory path is the schema, its components under /. Ported from the
// host's TestForDialect, its row "ydb is case-sensitive and has no default
// schema".
func TestDescriptionStatesExactIdentifiers_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	rules := backend.Description().GetEngines()[0].GetIdentifiers()

	c.Assert(rules, qt.CmpEquals(protocmp.Transform()), &protocol.IdentifierRules{
		TableNames: protocol.IdentifierRules_EXACT, ColumnNames: protocol.IdentifierRules_EXACT, IndexNames: protocol.IdentifierRules_EXACT,
		IndexNamespace: protocol.IdentifierRules_TABLE, SchemaSeparator: "/",
	})
}

// Two names that differ only in case name two objects: a source that declares
// node and Node in one directory declares two nodes, where it refuses one name
// declared twice.
func TestSourceKeepsNamesThatDifferInCase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request, err := testsource.Request(`{"coordination_nodes":{"a":{"name":"Node","schema":"app"},"b":{"name":"node","schema":"app"}}}`)
	c.Assert(err, qt.IsNil)

	decoded, err := backend.New().DecodeSource(c.Context(), request)

	c.Assert(err, qt.IsNil)
	var names []string
	for _, value := range decoded.Documents[0].Fragments[0].Values {
		names = append(names, value.Value.Value.Owner.Name)
	}
	c.Assert(names, qt.DeepEquals, []string{"Node", "node"})
}
