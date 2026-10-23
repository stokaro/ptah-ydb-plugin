//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// validationRequest declares one table with the given key columns, captured as
// the host captures it: exact identifiers and complete empty feature state.
func validationRequest(client interface {
	Description() *protocol.Description
}, name string, primaryKey []string, columns ...*protocol.DeclaredColumn) *protocol.ValidateSchemaRequest {
	empty := func() *protocol.ComparisonState {
		return &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}
	}
	part := func(value string) *protocol.ComparisonPart {
		return &protocol.ComparisonPart{Source: value, Normalized: value}
	}
	return &protocol.ValidateSchemaRequest{Engine: "ydb", Capabilities: map[string]bool{"transactional_ddl": false}, NoSkipped: true,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(client.Description().Engines[0].Identifiers)},
		Features:    &protocol.CapturedFeatures{Objects: empty(), Facets: empty()},
		Tables: []*protocol.ValidationTable{{Subject: &protocol.ComparisonIdentity{Kind: "table", Schema: part("app"), Name: part(name)},
			Declaration: &protocol.DeclaredTable{Name: name, Schema: "app", PrimaryKey: primaryKey, Columns: columns,
				Features: &protocol.CapturedFeatures{Objects: empty(), Facets: empty()}}}}}
}

// Validation reads only the request, so no database session is opened. The
// subprocess is the one the host starts: it must advertise the service and
// answer through the runtime's checked client.
func TestSchemaValidationOfflineSubprocess_HappyPath(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	c.Assert(client.Description().Features, qt.Contains, "schema.validate")
	request := validationRequest(client, "records", []string{"id"}, &protocol.DeclaredColumn{Name: "id", Type: "Uint64", Primary: true})
	original := proto.CloneOf(request)
	result, err := client.ValidateSchema(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Complete, qt.IsTrue)
	c.Assert(result.Diagnostics, qt.HasLen, 0)
	c.Assert(proto.Equal(request, original), qt.IsTrue)
}

func TestSchemaValidationOfflineSubprocess_FailurePath(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	request := validationRequest(client, "keyless", nil, &protocol.DeclaredColumn{Name: "id", Type: "Uint64"})
	result, err := client.ValidateSchema(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Complete, qt.IsTrue)
	c.Assert(result.Diagnostics, qt.HasLen, 1)
	c.Assert(result.Diagnostics[0].Code, qt.Equals, protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE)
	c.Assert(result.Diagnostics[0].Object, qt.Equals, "app/keyless")
	c.Assert(result.Diagnostics[0].Message, qt.Matches, `YDB table creation requires an ordered primary key`)
}
