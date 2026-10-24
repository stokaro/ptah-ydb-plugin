package schemavalidation_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// columnTable declares app.events as a column table with the given storage.
func columnTable(c *qt.C, storage string, columns ...*protocol.DeclaredColumn) *protocol.ValidationTable {
	c.Helper()
	declared := table("app", "events", columns...)
	model := schema.NativeDesiredModel(schema.ColumnStorageProperties)
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "storage",
		Representation: protocol.ValueInput_DESIRED, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: model.Kind, SchemaVersion: 1, Encoding: model.Encoding,
			DefinitionSha256: model.DefinitionSha256, Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "events"}, Payload: []byte(storage)}}}})
	c.Assert(err, qt.IsNil)
	declared.Declaration.Features.Facets.Records = []*protocol.ComparisonRecord{{Kind: model.Kind, Subject: subject("app", "events"), Value: values.Values[0]}}
	return declared
}

// TestValidateSchema_ColumnTable holds a column table to the rules its CREATE
// TABLE compiles with.
func TestValidateSchema_ColumnTable(t *testing.T) {
	c := qt.New(t)
	c.Assert(diagnostics(c, newRequest(columnTable(c, `{"hashColumns":["id"],"partitions":"2"}`, column("id", "Uint64", true), column("body", "Utf8", false)))), qt.HasLen, 0)
	refused := diagnostics(c, newRequest(columnTable(c, `{"hashColumns":["body"]}`, column("id", "Uint64", true), column("body", "Utf8", false))))
	c.Assert(refused, qt.HasLen, 1)
	c.Assert(refused[0].Code, qt.Equals, protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE)
	c.Assert(refused[0].Message, qt.Equals, `hash column "body" must belong to the primary key`)
}
