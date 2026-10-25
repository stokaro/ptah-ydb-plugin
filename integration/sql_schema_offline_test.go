//go:build integration

package integration_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// sqlSchemaRequest is the request the host sends for a load of YQL documents,
// as the SDK client's description of the provider states the engine.
func sqlSchemaRequest(description *protocol.Description, documents ...string) *protocol.ReadSQLSchemaRequest {
	engine := description.GetEngines()[0]
	request := &protocol.ReadSQLSchemaRequest{Engine: engine.GetId(), TargetFeatures: slices.Clone(engine.GetFeatures()),
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(engine.GetIdentifiers()), DefaultSchema: engine.GetDefaultSchema()}}
	for i, text := range documents {
		id := string(rune('1' + i))
		request.Documents = append(request.Documents, &protocol.SQLDocument{Id: id, Path: "0" + id + ".sql", Text: text})
	}
	return request
}

// The executable reads a load of two YQL documents without a session: the
// second alters what the first declared, each document answers what it
// declares, and the coordination node arrives as the provider's own model.
func TestSQLSchemaOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	engine := client.Description().GetEngines()[0]
	c.Assert(engine.GetSqlSchema().GetMode(), qt.Equals, protocol.SQLSchemaDeclaration_DOCUMENT)
	request := sqlSchemaRequest(client.Description(),
		"CREATE TABLE `app/items` (id Uint64 NOT NULL, name Utf8, PRIMARY KEY (id));\nCREATE COORDINATION NODE `app/locks`;",
		"COMMENT ON COLUMN `app/items`.`name` IS 'the name';\nCREATE USER reader;\nGRANT SELECT ON `app/items` TO reader;")
	// No session or database URL is supplied anywhere in this process test.
	reply, err := client.ReadSQLSchema(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(reply.GetComplete(), qt.IsTrue)
	c.Assert(reply.GetTables(), qt.HasLen, 1)
	c.Assert(reply.GetTables()[0].GetSchema(), qt.Equals, "app")
	c.Assert(reply.GetTables()[0].GetColumns()[1].GetComment(), qt.Equals, "the name")
	c.Assert(reply.GetNativeObjects(), qt.HasLen, 1)
	c.Assert(reply.GetNativeObjects()[0].GetKind(), qt.Equals, schema.CoordinationProperties)
	c.Assert(reply.GetGrants()[0].GetTarget().GetKind(), qt.Equals, "table")
	c.Assert(reply.GetDeclarations(), qt.HasLen, 2)
	c.Assert(reply.GetDeclarations()[1].GetObjects()[0].GetName(), qt.Equals, "reader")
}

// A refused load comes back incomplete, with the refusal at the position the
// host's own reader names, and with nothing else: a syntax error, and a table
// setting a SQL schema reading has no home for.
func TestSQLSchemaOfflineSubprocess_FailurePath(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	for _, test := range []struct {
		name, text, message string
		code                protocol.SQLDiagnostic_Code
		offset              uint64
	}{
		{name: "syntax", text: "CREATE TABLE t (id Uint64, PRIMARY KEY (id));", code: protocol.SQLDiagnostic_SYNTAX, offset: 44,
			message: `YQL schema: nullable primary key column "id" cannot be represented by a desired schema; declare NOT NULL (got ";")`},
		{name: "table settings", text: "CREATE TABLE t (id Uint64 NOT NULL, ts Timestamp, PRIMARY KEY (id)) WITH (TTL = Interval('PT1H') ON ts);",
			code: protocol.SQLDiagnostic_UNSUPPORTED, offset: 0,
			message: "table `t` declares a TTL, which a SQL schema reading has no home for: the host refuses the settings of a table in a SQL schema reading"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			reply, err := client.ReadSQLSchema(c.Context(), sqlSchemaRequest(client.Description(), test.text))
			c.Assert(err, qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsFalse)
			c.Assert(reply.GetTables(), qt.HasLen, 0)
			c.Assert(reply.GetDiagnostics(), qt.HasLen, 1)
			diagnostic := reply.GetDiagnostics()[0]
			c.Assert(diagnostic.GetCode(), qt.Equals, test.code)
			c.Assert(diagnostic.GetSpan().GetStart().GetByteOffset(), qt.Equals, test.offset)
			c.Assert(diagnostic.GetMessage(), qt.Equals, test.message)
		})
	}
}
