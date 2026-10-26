package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/declarationplan"
	"github.com/stokaro/ptah-ydb-plugin/internal/externalsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
)

func TestDatabaseContextResolvesWithoutCredentialsOrNetwork(t *testing.T) {
	for _, test := range []struct {
		name, address, database, source string
	}{
		{"environment credentials", "ydb://127.0.0.1:1/local?use_env_credentials", "/local", "/local"},
		{"realm", "ydb://127.0.0.1:1/local?dev_realm=context_proof", "/local/ptah_dev/context_proof", "/local"},
		{"static credentials", "ydbs://reader:DO_NOT_CAPTURE@127.0.0.1:1/local", "/local", "/local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			result, err := backend.New().ResolveDatabaseContext(c.Context(), &protocol.DatabaseContextRequest{Engine: "ydb", Address: test.address})
			c.Assert(err, qt.IsNil)
			c.Assert(result, qt.CmpEquals(protocmp.Transform()), &protocol.DatabaseContext{Database: test.database, SourceDatabase: test.source})
		})
	}
}

func TestDatabaseContextRefusalsDoNotRepeatAddresses(t *testing.T) {
	for _, address := range []string{"ydb://reader:DO_NOT_CAPTURE@host/local?unknown=DO_NOT_CAPTURE", "ydb://host/local/../other?token=DO_NOT_CAPTURE"} {
		c := qt.New(t)
		result, err := backend.New().ResolveDatabaseContext(c.Context(), &protocol.DatabaseContextRequest{Engine: "ydb", Address: address})
		c.Assert(err, qt.IsNotNil)
		c.Assert(result, qt.IsNil)
		c.Assert(err.Error(), qt.Not(qt.Contains), "DO_NOT_CAPTURE")
	}
	c := qt.New(t)
	ctx, cancel := context.WithCancel(c.Context())
	cancel()
	result, err := backend.New().ResolveDatabaseContext(ctx, &protocol.DatabaseContextRequest{Engine: "ydb", Address: "ydb://host/local"})
	c.Assert(err, qt.IsNotNil)
	c.Assert(result, qt.IsNil)
}

func normalizeContextValue(c *qt.C, value *protocol.Envelope, representation protocol.ValueInput_Representation) *protocol.NormalizedValue {
	c.Helper()
	request := &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: backend.Description().Engines[0].Features,
		Values: []*protocol.ValueInput{{Id: "context", Representation: representation, Value: value}}}
	result, err := backend.New().NormalizeValues(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateNormalizedValues(backend.Description(), request, result), qt.IsNil)
	return result.Values[0]
}

func TestAbsoluteSecretDependencyWaitsForContextWithoutChangingIdentity(t *testing.T) {
	c := qt.New(t)
	value := authoredSecretSource(c, "/local/app/z.pw")
	// A database in the object identity cannot authorize path interpretation.
	value.Owner.Database = "/identity"
	unbound := normalizeContextValue(c, value, protocol.ValueInput_DESIRED)
	c.Assert(unbound.RequiresDatabaseContext, qt.IsTrue)
	c.Assert(unbound.RequiredObjects, qt.HasLen, 0)
	before := proto.CloneOf(value)
	value.Context = &protocol.DatabaseContext{Database: "/local/.ptah-dev/proof", SourceDatabase: "/local"}
	bound := normalizeContextValue(c, value, protocol.ValueInput_DESIRED)
	c.Assert(bound.RequiresDatabaseContext, qt.IsFalse)
	c.Assert(bound.Value.Owner, qt.CmpEquals(protocmp.Transform()), before.Owner)
	c.Assert(bound.CanonicalProjection, qt.DeepEquals, unbound.CanonicalProjection)
	c.Assert(bound.Value.Context, qt.CmpEquals(protocmp.Transform()), value.Context)
	c.Assert(bound.RequiredObjects, qt.CmpEquals(protocmp.Transform()), []*protocol.ComparisonIdentity{{Kind: schema.SecretProperties,
		Database: &protocol.ComparisonPart{Source: "/identity", Normalized: "/identity"}, Schema: &protocol.ComparisonPart{Source: "app", Normalized: "app"}, Name: &protocol.ComparisonPart{Source: "z.pw", Normalized: "z.pw"}}})
	c.Assert(unbound.Value.Context, qt.IsNil)
}

func TestContextDependencyFailureDiscardsCompleteBatch(t *testing.T) {
	for _, root := range []string{"/other", "/local-other", "/local/../other"} {
		c := qt.New(t)
		value := authoredSecretSource(c, "/local/app/z.pw")
		value.Context = &protocol.DatabaseContext{Database: root, SourceDatabase: root}
		result, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", TargetFeatures: backend.Description().Engines[0].Features,
			Values: []*protocol.ValueInput{{Id: "valid", Representation: protocol.ValueInput_DESIRED, Value: authoredSecret(c)}, {Id: "invalid", Representation: protocol.ValueInput_DESIRED, Value: value}}})
		c.Assert(err, qt.IsNotNil)
		c.Assert(result, qt.IsNil)
	}
}

func TestExternalContextConversionRetainsDependencyMeaning(t *testing.T) {
	c := qt.New(t)
	value := authoredSecretSource(c, "/local/app/z.pw")
	value.Context = &protocol.DatabaseContext{Database: "/local/ptah_dev/proof", SourceDatabase: "/local"}
	desired := normalizeContextValue(c, value, protocol.ValueInput_DESIRED)
	request := &protocol.ConvertValuesRequest{Engine: "ydb", TargetFeatures: backend.Description().Engines[0].Features,
		From: protocol.ValueInput_DESIRED, To: protocol.ValueInput_OBSERVED, Values: []*protocol.NormalizedValue{desired}}
	observed, err := backend.New().ConvertValues(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(observed.Values[0].RequiredObjects, qt.CmpEquals(protocmp.Transform()), desired.RequiredObjects)
	c.Assert(observed.Values[0].Value.Context, qt.CmpEquals(protocmp.Transform()), value.Context)
	read, err := schema.ObservedExternalSource(observed.Values[0].Value)
	c.Assert(err, qt.IsNil)
	c.Assert(read.Properties["PASSWORD_SECRET_PATH"], qt.Equals, "/local/ptah_dev/proof/app/z.pw")
	request.From, request.To, request.Values = protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED, observed.Values
	restored, err := backend.New().ConvertValues(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(restored.Values[0], qt.CmpEquals(protocmp.Transform()), desired)
}

func TestCapturedExternalOperationsRetainOperandContext(t *testing.T) {
	c := qt.New(t)
	value := authoredSecretSource(c, "/local/app/z.pw")
	value.Context = &protocol.DatabaseContext{Database: "/local/ptah_dev/proof", SourceDatabase: "/local"}
	change, err := schema.CaptureExternalChange(schema.ExternalSourceProperties, "create", value.Owner, value.Context.Database, nil, value)
	c.Assert(err, qt.IsNil)
	operation, err := schema.ExternalOperation("create", change.Value, schema.ExternalCreate)
	c.Assert(err, qt.IsNil)
	c.Assert(change.Value.Context, qt.CmpEquals(protocmp.Transform()), value.Context)
	c.Assert(operation.Value.Context, qt.CmpEquals(protocmp.Transform()), value.Context)
	restored, _, err := schema.ExternalOperationChange(operation.Value)
	c.Assert(err, qt.IsNil)
	before, after, root, err := schema.ExternalOperands(restored)
	c.Assert(err, qt.IsNil)
	c.Assert(before, qt.IsNil)
	c.Assert(after, qt.CmpEquals(protocmp.Transform()), value)
	c.Assert(root, qt.Equals, value.Context.Database)
	authored, err := declarationplan.Capture("authored", value)
	c.Assert(err, qt.IsNil)
	c.Assert(authored.Value.Context, qt.CmpEquals(protocmp.Transform()), value.Context)
	declaration, err := declarationplan.Declaration(authored.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(declaration, qt.CmpEquals(protocmp.Transform()), value)
	operation.Value.Context.Database = "/other"
	_, err = schema.NormalizeExternalOperation(&protocol.ValueInput{Id: "tampered", Representation: protocol.ValueInput_OPERATION, Value: operation.Value})
	c.Assert(err, qt.ErrorMatches, ".*context differs from its operands")
}

func TestSourceDecodeRetainsPendingAbsoluteDependencies(t *testing.T) {
	c := qt.New(t)
	request, err := testsource.Request(`{"coordination_nodes":{"source":{"schema":"app","source_type":"PostgreSQL","location":"pg:5432","auth_method":"BASIC","options":{"PASSWORD_SECRET_PATH":"/local/app/z.pw","TOKEN_SECRET_PATH":"app/token"}}}}`)
	c.Assert(err, qt.IsNil)
	request.TargetFeatures = backend.Description().Engines[0].Features
	document := request.Documents[0]
	document.Models = []*protocol.ValueModel{schema.NativeDesiredModel(schema.ExternalSourceProperties)}
	document.Fragments[0].YamlKey = externalsource.YAMLKeys()[0]
	document.Fragments[0].Selector = document.Fragments[0].YamlKey.Selector
	decoded, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	value := decoded.Documents[0].Fragments[0].Values[0].Value
	c.Assert(value.RequiresDatabaseContext, qt.IsTrue)
	c.Assert(value.RequiredObjects, qt.HasLen, 0)
	bound := proto.CloneOf(value.Value)
	bound.Context = &protocol.DatabaseContext{Database: "/local", SourceDatabase: "/local"}
	normalized := normalizeContextValue(c, bound, protocol.ValueInput_DESIRED)
	c.Assert(normalized.RequiresDatabaseContext, qt.IsFalse)
	c.Assert(normalized.RequiredObjects, qt.HasLen, 2)
}
