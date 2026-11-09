package yqlschema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
)

// undeclaredRequest reads the YQL document text for a host that lists the
// given features of the engine.
func undeclaredRequest(text string, features []string) *protocol.ReadSQLSchemaRequest {
	engine := backend.Description().GetEngines()[0]
	return &protocol.ReadSQLSchemaRequest{Engine: engine.GetId(), TargetFeatures: features,
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(engine.GetIdentifiers()), DefaultSchema: engine.GetDefaultSchema()},
		Documents:   []*protocol.SQLDocument{{Id: "1", Path: "feed.sql", Text: text}}}
}

// undeclaredStatements change an object no document of the load declares.
var undeclaredStatements = []struct{ name, text, message string }{
	{name: "a changefeed of a table", text: "ALTER TABLE users ADD CHANGEFEED updates WITH (mode='UPDATES', format='JSON');\n",
		message: "the schema model has no place for this statement: ALTER TABLE users ADD CHANGEFEED updates names a table this schema does not declare"},
	{name: "a comment on a table", text: "COMMENT ON TABLE `users` IS 'people';\n",
		message: "the schema model has no place for this statement: COMMENT ON TABLE users names an object this schema does not declare"},
	{name: "a consumer of a topic", text: "ALTER TOPIC `events` ADD CONSUMER worker;\n",
		message: "the schema model has no place for this statement: ALTER TOPIC events ADD CONSUMER names no topic or changefeed this schema declares"},
	{name: "a user", text: "ALTER USER app WITH PASSWORD 'Later1pw';\n",
		message: "the schema model has no place for this statement: ALTER USER requires a user declared earlier in this desired schema"},
}

// Under schema.read-sql-undeclared a statement that changes an object no
// document of the load declares is an UNDECLARED error, which the host reads
// as its own reader's refusal of such a statement and explains beside a
// source in another format, as it explained the built-in engine's refusal at
// 74de0b65b. The message stays the built-in engine's.
func TestReplyReportsAnUndeclaredStatement_HappyPath(t *testing.T) {
	for _, test := range undeclaredStatements {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := undeclaredRequest(test.text, []string{provider.SQLUndeclaredFeature})

			reply := yqlschema.Reply(request)

			c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsFalse)
			c.Assert(reply.GetDiagnostics(), qt.HasLen, 1)
			c.Assert(reply.GetDiagnostics()[0].GetCode(), qt.Equals, protocol.SQLDiagnostic_UNDECLARED)
			c.Assert(reply.GetDiagnostics()[0].GetSeverity(), qt.Equals, protocol.SQLDiagnostic_ERROR)
			c.Assert(reply.GetDiagnostics()[0].GetMessage(), qt.Equals, test.message)
		})
	}
}

// A host that does not list the feature gets the INVALID error it got
// before the code existed, which the SDK refuses UNDECLARED for, and a
// statement refused for another reason keeps its code under the feature.
func TestReplyReportsAnUndeclaredStatementAsInvalid_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, text string
		features   []string
	}{
		{name: "an undeclared statement for a host without the feature", text: undeclaredStatements[0].text},
		{name: "a changefeed declared twice", text: "CREATE TABLE users (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
			"ALTER TABLE users ADD CHANGEFEED updates WITH (mode='UPDATES', format='JSON');\n" +
			"ALTER TABLE users ADD CHANGEFEED updates WITH (mode='UPDATES', format='JSON');\n", features: []string{provider.SQLUndeclaredFeature}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := undeclaredRequest(test.text, test.features)

			reply := yqlschema.Reply(request)

			c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetDiagnostics(), qt.HasLen, 1)
			c.Assert(reply.GetDiagnostics()[0].GetCode(), qt.Equals, protocol.SQLDiagnostic_INVALID)
		})
	}
}
