package schemavalidation_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/schemavalidation"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

func emptyFeatures() *protocol.CapturedFeatures {
	empty := func() *protocol.ComparisonState {
		return &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}}
	}
	return &protocol.CapturedFeatures{Objects: empty(), Facets: empty()}
}

func subject(schemaName, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "table", Schema: testcomparison.Part(schemaName), Name: testcomparison.Part(name)}
}

func table(schemaName, name string, columns ...*protocol.DeclaredColumn) *protocol.ValidationTable {
	return &protocol.ValidationTable{Subject: subject(schemaName, name), Declaration: &protocol.DeclaredTable{
		Name: name, Schema: schemaName, PrimaryKey: []string{"id"}, Columns: columns, Features: emptyFeatures()}}
}

func column(name, kind string, primary bool) *protocol.DeclaredColumn {
	return &protocol.DeclaredColumn{Name: name, Type: kind, Primary: primary}
}

func newRequest(tables ...*protocol.ValidationTable) *protocol.ValidateSchemaRequest {
	return &protocol.ValidateSchemaRequest{Engine: "ydb", Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
		Capabilities: map[string]bool{"transactional_ddl": false}, Tables: tables, Features: emptyFeatures()}
}

func keyed(schemaName, name string) *protocol.ValidationTable {
	return table(schemaName, name, column("id", "Uint64", true), column("body", "Utf8", false))
}

// diagnostics runs the provider service the way the runtime does and checks
// that the SDK accepts the reply.
func diagnostics(c *qt.C, request *protocol.ValidateSchemaRequest) []*protocol.SchemaDiagnostic {
	c.Helper()
	original := proto.CloneOf(request)
	result, err := backend.New().ValidateSchema(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Complete, qt.IsTrue)
	c.Assert(provider.ValidateSchemaResult(backend.Description(), request, result), qt.IsNil)
	c.Assert(proto.Equal(request, original), qt.IsTrue, qt.Commentf("validation changed its request"))
	return result.Diagnostics
}

func TestValidateSchema_HappyPath(t *testing.T) {
	t.Run("an empty schema completes", func(t *testing.T) {
		c := qt.New(t)
		c.Assert(diagnostics(c, newRequest()), qt.HasLen, 0)
	})
	t.Run("keyed tables in nested directories", func(t *testing.T) {
		c := qt.New(t)
		request := newRequest(keyed("", "plain"), keyed("app/sub", "nested"), keyed("app", "sibling"))
		c.Assert(diagnostics(c, request), qt.HasLen, 0)
	})
	t.Run("a composite key", func(t *testing.T) {
		c := qt.New(t)
		composite := table("", "pairs", column("a", "Uint64", true), column("b", "Utf8", true))
		composite.Declaration.PrimaryKey = []string{"a", "b"}
		c.Assert(diagnostics(c, newRequest(composite)), qt.HasLen, 0)
	})
	t.Run("directory settings are accepted unless nothing may be skipped", func(t *testing.T) {
		c := qt.New(t)
		request := newRequest(keyed("app", "items"))
		request.Namespaces = []*protocol.DeclaredNamespace{{Subject: &protocol.ComparisonIdentity{Kind: "schema", Name: testcomparison.Part("app")}, Name: "app", Comment: "notes"}}
		c.Assert(diagnostics(c, request), qt.HasLen, 0)
	})
	t.Run("standalone enums are accepted unless nothing may be skipped", func(t *testing.T) {
		c := qt.New(t)
		request := newRequest()
		request.Enums = []*protocol.DeclaredEnum{{Name: "mood", Values: []string{"a", "b"}}}
		c.Assert(diagnostics(c, request), qt.HasLen, 0)
	})
}

func TestValidateSchema_Diagnostics(t *testing.T) {
	for _, test := range []struct {
		name    string
		request func() *protocol.ValidateSchemaRequest
		code    protocol.SchemaDiagnostic_Code
		kind    string
		object  string
		message string
	}{
		{"no primary key", func() *protocol.ValidateSchemaRequest {
			bare := table("app", "bare", column("id", "Uint64", false))
			bare.Declaration.PrimaryKey = nil
			return newRequest(bare)
		}, protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, "table", "app/bare", `YDB table creation requires an ordered primary key`},
		{"no columns", func() *protocol.ValidateSchemaRequest { return newRequest(table("", "hollow")) },
			protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, "table", "hollow", `YDB table creation requires columns`},
		{"unknown type", func() *protocol.ValidateSchemaRequest {
			return newRequest(table("", "typed", column("id", "Uint64", true), column("b", "Wibble", false)))
		}, protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, "table", "typed", `unsupported YDB storage column type`},
		{"key names no column", func() *protocol.ValidateSchemaRequest {
			misdirected := keyed("", "misdirected")
			misdirected.Declaration.PrimaryKey = []string{"nope"}
			return newRequest(misdirected)
		}, protocol.SchemaDiagnostic_INVALID_SCHEMA, "table", "misdirected", `invalid YDB primary key column`},
		{"common index", func() *protocol.ValidateSchemaRequest {
			indexed := keyed("", "indexed")
			indexed.Declaration.Indexes = []*protocol.DeclaredIndex{{Name: "by_body", Fields: []string{"body"}}}
			return newRequest(indexed)
		}, protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, "table", "indexed", `YDB table creation does not support .*indexes`},
		{"table inside a table", func() *protocol.ValidateSchemaRequest {
			return newRequest(keyed("", "a"), keyed("a", "b"))
		}, protocol.SchemaDiagnostic_INVALID_SCHEMA, "table path", "a/b", `YDB table a/b lies inside table a; a table holds no children`},
		{"directory with dots", func() *protocol.ValidateSchemaRequest {
			request := newRequest()
			request.Namespaces = []*protocol.DeclaredNamespace{{Subject: &protocol.ComparisonIdentity{Kind: "schema", Name: testcomparison.Part("a/../b")}, Name: "a/../b"}}
			return request
		}, protocol.SchemaDiagnostic_INVALID_SCHEMA, "schema", "a/../b", `invalid YDB directory declaration`},
		{"directory comment cannot be skipped", func() *protocol.ValidateSchemaRequest {
			request := newRequest()
			request.NoSkipped = true
			request.Namespaces = []*protocol.DeclaredNamespace{{Subject: &protocol.ComparisonIdentity{Kind: "schema", Name: testcomparison.Part("app")}, Name: "app", Comment: "notes"}}
			return request
		}, protocol.SchemaDiagnostic_OMITTED_DECLARATION, "schema", "app", `comment=notes would be skipped`},
		{"enum cannot be skipped", func() *protocol.ValidateSchemaRequest {
			request := newRequest()
			request.NoSkipped = true
			request.Enums = []*protocol.DeclaredEnum{{Name: "mood", Values: []string{"a"}}}
			return request
		}, protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, "enum", "mood", `enum mood, which requires .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			got := diagnostics(c, test.request())
			c.Assert(got, qt.HasLen, 1)
			c.Assert(got[0].Code, qt.Equals, test.code)
			c.Assert(got[0].Kind, qt.Equals, test.kind)
			c.Assert(got[0].Object, qt.Equals, test.object)
			c.Assert(got[0].Message, qt.Matches, test.message)
		})
	}
}

func TestValidateSchema_ReportsEveryRefusalInADeterministicOrder(t *testing.T) {
	c := qt.New(t)
	request := newRequest(
		table("", "zeta", column("id", "Uint64", true), column("b", "Wibble", false)),
		table("", "alpha", column("id", "Uint64", true), column("b", "Wibble", false)),
	)
	reversed := proto.CloneOf(request)
	reversed.Tables[0], reversed.Tables[1] = reversed.Tables[1], reversed.Tables[0]
	first, second := diagnostics(c, request), diagnostics(c, reversed)
	c.Assert(first, qt.HasLen, 2)
	c.Assert(first[0].Object, qt.Equals, "alpha")
	c.Assert(first[1].Object, qt.Equals, "zeta")
	c.Assert(len(first), qt.Equals, len(second))
	for i := range first {
		c.Assert(proto.Equal(first[i], second[i]), qt.IsTrue)
	}
}

func TestValidateSchema_AccessRequirementsAndTemporaryTablesAreValidDeclarations(t *testing.T) {
	for _, payload := range []string{`{"temporary":true}`, `{"access":{}}`} {
		t.Run(payload, func(t *testing.T) {
			c := qt.New(t)
			declared := keyed("", "settings")
			declared.Declaration.Features = tableFacet(c, "settings", payload)
			c.Assert(diagnostics(c, newRequest(declared)), qt.HasLen, 0)
		})
	}
}

func tableFacet(c *qt.C, name, payload string) *protocol.CapturedFeatures {
	c.Helper()
	reference := &protocol.Reference{Engine: "ydb", Kind: "table", Name: name}
	model := schema.TableDesiredModel()
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: "table", Representation: protocol.ValueInput_DESIRED, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.TableProperties,
			SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: reference, Payload: []byte(payload)}}}})
	c.Assert(err, qt.IsNil)
	features := emptyFeatures()
	features.Facets.Records = []*protocol.ComparisonRecord{{Kind: schema.TableProperties, Subject: subject("", name), Value: values.Values[0]}}
	return features
}

func TestValidateSchema_FailurePath(t *testing.T) {
	t.Run("a canceled context returns no diagnostics", func(t *testing.T) {
		c := qt.New(t)
		ctx, cancel := context.WithCancel(c.Context())
		cancel()
		result, err := backend.New().ValidateSchema(ctx, newRequest(keyed("", "items")))
		c.Assert(err, qt.ErrorMatches, `.*context canceled.*`)
		c.Assert(result, qt.IsNil)
		direct, err := schemavalidation.Validate(ctx, newRequest())
		c.Assert(err, qt.ErrorIs, context.Canceled)
		c.Assert(direct, qt.IsNil)
	})
	t.Run("an identifier context the provider cannot interpret", func(t *testing.T) {
		c := qt.New(t)
		request := newRequest(keyed("", "items"))
		request.Identifiers.DefaultSchema = "public"
		result, err := backend.New().ValidateSchema(c.Context(), request)
		c.Assert(err, qt.ErrorMatches, `.*`)
		c.Assert(result, qt.IsNil)
	})
	t.Run("a request without the service negotiated", func(t *testing.T) {
		c := qt.New(t)
		result, err := backend.New().ValidateSchema(c.Context(), nil)
		c.Assert(err, qt.ErrorMatches, `.*schema validation is unavailable.*`)
		c.Assert(result, qt.IsNil)
	})
}
