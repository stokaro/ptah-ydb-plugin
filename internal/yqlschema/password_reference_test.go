package yqlschema_test

import (
	"slices"
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

// passwordRequest reads text as a document of a host with the engine's
// features, those of source.sql-role-passwords among them unless without
// says to leave it out.
func passwordRequest(text string, without bool) *protocol.ReadSQLSchemaRequest {
	description := backend.Description()
	engine := description.GetEngines()[0]
	features := slices.DeleteFunc(slices.Clone(engine.GetFeatures()), func(feature string) bool {
		return without && feature == provider.SQLRolePasswordsFeature
	})
	return &protocol.ReadSQLSchemaRequest{Engine: engine.GetId(), TargetFeatures: features,
		Identifiers:  &protocol.ComparisonIdentifiers{Rules: engine.GetIdentifiers(), DefaultSchema: engine.GetDefaultSchema()},
		DatabasePath: "/local", Documents: []*protocol.SQLDocument{{Id: "1", Path: "schema.sql", Text: text}}}
}

// Under source.sql-role-passwords the reply carries the password a document
// gives a user as the document writes it: a literal, a YDB password hash
// after HASH, or the reference a migration file Ptah generated names it by.
// The host binds it at execution as it binds one a YAML source declares; the
// host's embedded engine read the same declarations (G64).
func TestReplyPasswords_HappyPath(t *testing.T) {
	const hash = `{"hash":"aGFzaA==","salt":"c2FsdA==","type":"argon2id"}`
	for _, test := range []struct {
		name, text, want string
	}{
		{name: "a literal", text: "CREATE USER app PASSWORD 'Secret1pw';", want: "Secret1pw"},
		{name: "a hash", text: "CREATE USER app HASH '" + hash + "';", want: hash},
		{name: "a reference", text: "CREATE USER app PASSWORD $PTAH_SECRET_PASSWORD_APP;", want: "$PTAH_SECRET_PASSWORD_APP"},
		{name: "one an ALTER USER sets", text: "CREATE USER app; ALTER USER app WITH PASSWORD 'Later1pw';", want: "Later1pw"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := passwordRequest(test.text, false)

			reply := yqlschema.Reply(request)

			c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
			c.Assert(reply.GetComplete(), qt.IsTrue, qt.Commentf("%v", reply.GetDiagnostics()))
			c.Assert(reply.GetRoles(), qt.HasLen, 1)
			c.Assert(reply.GetRoles()[0].GetPasswordDeclared(), qt.IsTrue)
			c.Assert(reply.GetRoles()[0].GetPassword(), qt.Equals, test.want)
		})
	}
}

// A user without a password, or with PASSWORD NULL, declares none.
func TestReplyPasswordsWithoutOne_HappyPath(t *testing.T) {
	for _, text := range []string{"CREATE USER app;", "CREATE USER app PASSWORD NULL;"} {
		t.Run(text, func(t *testing.T) {
			c := testcheck.New(t)

			reply := yqlschema.Reply(passwordRequest(text, false))

			c.Assert(reply.GetComplete(), qt.IsTrue)
			c.Assert(reply.GetRoles()[0].GetPasswordDeclared(), qt.IsFalse)
			c.Assert(reply.GetRoles()[0].GetPassword(), qt.Equals, "")
		})
	}
}

// A host that does not take the password a document gives a role has no home
// for it, so a user read with a reference is refused by name, as one read with
// a literal is, and the refusal holds no variable either.
func TestReplyPasswordReference_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := passwordRequest("CREATE USER app PASSWORD $PTAH_SECRET_PASSWORD_APP;", true)

	reply := yqlschema.Reply(request)

	c.Assert(provider.ValidateSQLSchema(backend.Description(), request, reply), qt.IsNil)
	c.Assert(reply.GetComplete(), qt.IsFalse)
	c.Assert(reply.GetDiagnostics(), qt.HasLen, 1)
	c.Assert(reply.GetDiagnostics()[0].GetMessage(), qt.Contains, "user `app` declares a password, which a SQL schema reading has no home for")
	c.Assert(reply.GetDiagnostics()[0].GetMessage(), qt.Not(qt.Contains), "PTAH_SECRET")
}
