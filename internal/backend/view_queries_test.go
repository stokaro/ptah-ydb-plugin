package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// viewQueryRequest asks whether declared is the query of the view app/v,
// which the database holds with held.
func viewQueryRequest(declared, held string) *protocol.CompareViewQueriesRequest {
	engine := backend.Description().GetEngines()[0]
	return &protocol.CompareViewQueriesRequest{Engine: engine.GetId(), TargetFeatures: engine.GetFeatures(),
		Identifiers: &protocol.ComparisonIdentifiers{Rules: engine.GetIdentifiers(), DefaultSchema: engine.GetDefaultSchema()},
		Pairs: []*protocol.ViewQueryPair{{Declared: declared,
			Current: &protocol.ObservedView{Identity: &protocol.Reference{Engine: engine.GetId(), Schema: "app", Kind: "view", Name: "v"}, Body: held}}}}
}

// The provider and the engine list schema.view-query-equivalence, so the
// host asks the provider about every view query of YDB.
func TestDescriptionListsViewQueryEquivalence(t *testing.T) {
	c := testcheck.New(t)

	description := backend.Description()

	c.Assert(description.GetFeatures(), qt.Contains, provider.ViewQueryEquivalenceFeature)
	c.Assert(description.GetEngines()[0].GetFeatures(), qt.Contains, provider.ViewQueryEquivalenceFeature)
}

// A declared view query and the held one are one query when they read the
// same in the form YDB stores a query in: the server drops comments, line
// breaks and terminators and spaces the tokens, and keeps the case of every
// word and the contents of a string. So a change of case alone, which the
// host's shared reading folds away, is another query, as the host's embedded
// engine reported it (G204), and a keyword's case is kept too, as the server
// keeps it.
func TestCompareViewQueries_HappyPath(t *testing.T) {
	const held = "SELECT id AS Answer FROM `app/t` WHERE label = 'x'"
	tests := []struct {
		name     string
		declared string
		want     bool
	}{
		{name: "the same text", declared: held, want: true},
		{name: "spacing, a comment and a terminator", declared: "SELECT id AS Answer -- the answer\n  FROM `app/t`\n  WHERE label='x';", want: true},
		{name: "the case of an alias", declared: "SELECT id AS answer FROM `app/t` WHERE label = 'x'", want: false},
		{name: "the case of a keyword", declared: "select id AS Answer FROM `app/t` WHERE label = 'x'", want: false},
		{name: "the case inside a string", declared: "SELECT id AS Answer FROM `app/t` WHERE label = 'X'", want: false},
		{name: "another column", declared: "SELECT id AS Answer FROM `app/t` WHERE name = 'x'", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := viewQueryRequest(test.declared, held)

			reply, err := (&backend.Provider{}).CompareViewQueries(c.Context(), request)

			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateComparedViewQueries(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetQueries(), qt.HasLen, 1)
			c.Assert(reply.GetQueries()[0].GetIdentity().GetName(), qt.Equals, "v")
			c.Assert(reply.GetQueries()[0].GetEquivalent(), qt.Equals, test.want)
		})
	}
}

// A pair whose declared query is blank is refused before anything is
// compared, as the SDK's validation of the request refuses it.
func TestCompareViewQueries_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	reply, err := (&backend.Provider{}).CompareViewQueries(c.Context(), viewQueryRequest("  ", "SELECT 1"))

	c.Assert(reply, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(err, qt.ErrorMatches, `.*the pair of view "v" has no declared query.*`)
}
