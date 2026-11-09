//go:build integration

package integration_test

import (
	"fmt"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	providerruntime "ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// manyTables is how many tables a large schema declares: a directory of a
// database the built-in engine planned in one run, where the provider's first
// release refused 15.
const manyTables = 500

// startProviderWith starts the provider with the host features hostFeatures
// names and the ones given.
func startProviderWith(c *qt.C, features ...string) *providerruntime.Client {
	c.Helper()
	binary, digest := providerBinary(c)
	return startProviderListing(c, binary, digest, slices.Concat(hostFeatures(), features))
}

// largeValidationRequest declares count trivial tables, each captured as the
// host captures a table of a YAML or Go source: its state covers every
// desired model of the engine, complete, in each placement.
func largeValidationRequest(client *providerruntime.Client, count int) *protocol.ValidateSchemaRequest {
	covered := func() *protocol.ComparisonState {
		coverage := &protocol.ComparisonCoverage{}
		for _, model := range client.Description().Engines[0].ValueModels {
			if model.GetRepresentation() == protocol.ValueInput_DESIRED {
				coverage.Kinds = append(coverage.Kinds, &protocol.ComparisonKindCoverage{Model: model,
					Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}}})
			}
		}
		return &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: coverage}
	}
	request := validationRequest(client, "t0", []string{"id"}, &protocol.DeclaredColumn{Name: "id", Type: "Uint64", Primary: true})
	template := request.Tables[0]
	request.Tables = nil
	for i := range count {
		table := proto.CloneOf(template)
		name := fmt.Sprintf("t%04d", i)
		table.Subject.Name = &protocol.ComparisonPart{Source: name, Normalized: name}
		table.Declaration.Name = name
		table.Declaration.Features = &protocol.CapturedFeatures{Objects: covered(), Facets: covered()}
		request.Tables = append(request.Tables, table)
	}
	return request
}

// Every model of a table's coverage travels by reference and a request may be
// 64 MiB, so a schema of 500 tables is validated in one request, as the
// built-in engine validated it.
func TestSchemaValidationOfflineSubprocessOfManyTables_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderWith(c, provider.ModelReferencesFeature, provider.LargeMessagesFeature)
	request := largeValidationRequest(client, manyTables)

	result, err := client.ValidateSchema(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(result.Complete, qt.IsTrue)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
}

// A host that lists neither feature sends each model whole in every table's
// coverage, and the request does not fit the 4 MiB an older contract allows.
func TestSchemaValidationOfflineSubprocessOfManyTables_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	client := startProviderWith(c)
	request := largeValidationRequest(client, manyTables)

	result, err := client.ValidateSchema(c.Context(), request)

	c.Assert(err, qt.ErrorMatches, `provider validate schema failed \(ResourceExhausted\).*`)
	c.Assert(result, qt.IsNil)
}
