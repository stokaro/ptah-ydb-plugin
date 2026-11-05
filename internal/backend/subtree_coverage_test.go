package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// subtreeRegion is a region over the directory and every directory below it.
func subtreeRegion(directory string, knowledge *protocol.ModelKnowledge) *protocol.ComparisonRegion {
	return &protocol.ComparisonRegion{Scope: &protocol.ComparisonNamespace{Targets: []string{"ydb"}, SubjectKinds: []string{schema.SecretProperties},
		Database: testcomparison.Part("/local"), Catalog: testcomparison.Part(""), Schema: testcomparison.Part(directory), SchemaSubtree: true},
		Knowledge: knowledge}
}

var (
	complete = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}
	notRead  = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "not read"}
)

// A read that claims the root and everything below it complete establishes
// that a secret declared in directory app is absent, so the comparison
// creates it; one that also leaves the subtree of app unknown leaves the
// secret undecided.
func TestSecretComparisonReadsSubtreeRegions(t *testing.T) {
	for _, test := range []struct {
		name               string
		regions            []*protocol.ComparisonRegion
		changes, undecided int
	}{
		{name: "the root's subtree", regions: []*protocol.ComparisonRegion{subtreeRegion("", complete)}, changes: 1},
		{name: "the directory's own subtree", regions: []*protocol.ComparisonRegion{subtreeRegion("app", complete)}, changes: 1},
		{name: "an unknown subtree below the root's", regions: []*protocol.ComparisonRegion{
			subtreeRegion("", complete), subtreeRegion("app", notRead),
		}, undecided: 1},
		{name: "the subtree of a sibling sharing the prefix", regions: []*protocol.ComparisonRegion{subtreeRegion("ap", complete)}, undecided: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := secretRequest(c, false)
			request.Current.Coverage.Kinds[0].Sources[0].Regions = test.regions
			ctx := provider.ContextWithHostFeatures(c.Context(), []string{provider.SchemaSubtreeCoverageFeature})

			result, err := backend.New().CompareModels(ctx, request)

			c.Assert(err, qt.IsNil)
			c.Assert(result.Changes, qt.HasLen, test.changes)
			c.Assert(result.Undecided, qt.HasLen, test.undecided)
		})
	}
}

// The engine declares / as its schema separator and lists the feature, and a
// host that does not list it sends the rules without the separator, which the
// comparison still accepts.
func TestIdentifierRulesWithoutTheSubtreeFeature(t *testing.T) {
	c := testcheck.New(t)
	engine := backend.Description().Engines[0]
	request := secretRequest(c, false)
	request.Identifiers.Rules.SchemaSeparator = ""

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(engine.GetIdentifiers().GetSchemaSeparator(), qt.Equals, "/")
	c.Assert(engine.GetFeatures(), qt.Contains, provider.SchemaSubtreeCoverageFeature)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
}
