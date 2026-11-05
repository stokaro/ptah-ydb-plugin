package yqlschema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
)

// A migration file Ptah generated names a user's password by its variable
// (ADR 0071), and a reading of it keeps the reference as the password, so it
// names that variable and no value.
func TestReadPasswordReference_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	reading, refusal := yqlschema.Read([]yqlschema.Document{{Path: "schema.sql",
		Text: "CREATE USER app PASSWORD $PTAH_SECRET_PASSWORD_APP; ALTER USER app WITH PASSWORD $PTAH_SECRET_APP_PW NOLOGIN;"}}, "/local")

	c.Assert(refusal, qt.IsNil)
	c.Assert(reading.Roles, qt.HasLen, 1)
	c.Assert(reading.Roles[0].Password, qt.Equals, "$PTAH_SECRET_APP_PW")
}

// A reference outside the PTAH_SECRET_ prefix's exact spelling, or after
// HASH, is no password Ptah writes, and the declaration is refused in the
// host's words for a malformed user, which repeat no token of it.
func TestReadPasswordReference_FailurePath(t *testing.T) {
	for _, text := range []string{"CREATE USER app PASSWORD $ptah_secret_password_app;", "CREATE USER app PASSWORD $other;",
		"CREATE USER app HASH $PTAH_SECRET_PASSWORD_APP;"} {
		t.Run(text, func(t *testing.T) {
			c := testcheck.New(t)

			reading, refusal := yqlschema.Read([]yqlschema.Document{{Path: "schema.sql", Text: text}}, "/local")

			c.Assert(reading, qt.IsNil)
			c.Assert(refusal, qt.IsNotNil)
			c.Assert(refusal.Message, qt.Equals, "YQL schema: invalid user declaration; expected a user name and PASSWORD, HASH, LOGIN or NOLOGIN options")
		})
	}
}

// The SQL schema reply carries no password, so a user read with a reference
// is refused by name, as one read with a literal is, and the refusal holds no
// variable either.
func TestReplyPasswordReference_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	engine := description.GetEngines()[0]
	request := &protocol.ReadSQLSchemaRequest{Engine: engine.GetId(), TargetFeatures: engine.GetFeatures(),
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: engine.GetIdentifiers(), DefaultSchema: engine.GetDefaultSchema()},
		DatabasePath: "/local", Documents: []*protocol.SQLDocument{{Id: "1", Path: "schema.sql", Text: "CREATE USER app PASSWORD $PTAH_SECRET_PASSWORD_APP;"}}}

	reply := yqlschema.Reply(request)

	c.Assert(provider.ValidateSQLSchema(description, request, reply), qt.IsNil)
	c.Assert(reply.GetComplete(), qt.IsFalse)
	c.Assert(reply.GetDiagnostics(), qt.HasLen, 1)
	c.Assert(reply.GetDiagnostics()[0].GetMessage(), qt.Contains, "user `app` declares a password, which a SQL schema reading has no home for")
}
