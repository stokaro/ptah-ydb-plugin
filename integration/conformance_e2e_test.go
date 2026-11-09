//go:build integration

package integration_test

import (
	"encoding/json"
	"net/url"
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/conformance"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// providerID is the identity the provider describes and the official
// catalog names.
const providerID = "github.com/stokaro/ptah-ydb-plugin"

// ptahRequires is what Ptah requires of every provider.
var ptahRequires = []string{provider.InspectFeature, provider.RenderFeature, provider.ExecuteFeature, provider.AnalyzeSQLFeature}

// recordedModels are the model identities release v0.4.0 described, as
// ptah-provider-conformance models printed them. A file a host exported
// through that release names a model this way, so each must still read: a
// change of a model's definition keeps the identity it replaces as a
// predecessor. A release records its own identities here in place of these.
func recordedModels(c *qt.C) []conformance.ModelIdentity {
	c.Helper()
	content, err := os.ReadFile("testdata/model-identities-v0.4.0.json")
	c.Assert(err, qt.IsNil)
	var identities []conformance.ModelIdentity
	c.Assert(json.Unmarshal(content, &identities), qt.IsNil)
	c.Assert(identities, qt.Not(qt.HasLen), 0)
	return identities
}

// conformanceConfig is the offline configuration of the suite: requests to
// the services that need no database, the model identities of the last
// release, and an address whose credential is the suite's canary for the
// offline database context.
func conformanceConfig(c *qt.C) conformance.Config {
	c.Helper()
	binary, digest := providerBinary(c)
	source := "CREATE TABLE `app/t` (id Int64, PRIMARY KEY(id));\nDROP TOPIC `topic`;"
	return conformance.Config{
		Target:            conformance.Executable(binary, digest),
		ProviderID:        providerID,
		RequiredFeatures:  ptahRequires,
		Models:            recordedModels(c),
		CredentialAddress: "ydb://conformance:" + conformance.Canary + "@127.0.0.1:1/local",
		Calls: []conformance.Call{
			{Name: "analyze", Request: &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{source}}},
			{Name: "analyze-unclosed-string", Request: &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{"SELECT 'unclosed"}}, Refused: true},
			{Name: "scan", Request: &protocol.ScanSQLRequest{Engine: "ydb", Text: source, Head: true}},
			// The provider renders native operations and common objects, so a
			// common table creation is refused, and it says why.
			{Name: "render-common-table", Request: &protocol.RenderRequest{Engine: "ydb", Capabilities: map[string]bool{"transactional_ddl": false},
				Changes: []*protocol.Change{{Id: "create", Object: &protocol.Reference{Kind: "table", Name: "t"},
					Operation: &protocol.Change_CreateTable{CreateTable: &protocol.Table{Columns: []*protocol.Column{{Name: "id", Type: "Int64", PrimaryKey: true}}}}}}}, Refused: true},
		},
	}
}

// The provider passes Ptah's conformance suite with no database: the
// handshake, the description and its negotiation, every RPC the description
// offers or does not, offline calls under every host profile, and what its
// refusals say.
func TestConformanceOfflineSubprocess(t *testing.T) {
	c := testcheck.New(t)

	report, err := conformance.Check(c.Context(), conformanceConfig(c))

	c.Assert(err, qt.IsNil)
	c.Assert(report.Err(), qt.IsNil)
	c.Assert(report.Provider.ID, qt.Equals, providerID)
}

// The live part of the suite against the test server: sessions, reads,
// locks, transactions, a table created and dropped, a refused statement, and
// a login with the canary as its password. Reads are scoped to a directory of
// the test's own; the lock node the suite's locks create is removed with it.
func TestConformanceLive(t *testing.T) {
	c := testcheck.New(t)
	_, directory, database := lockFixture(c)
	table := path.Join(database, directory, "conformance")
	config := conformanceConfig(c)
	config.CredentialAddress = withCredential(c, os.Getenv("PTAH_YDB_TEST_URL"))
	config.Live = &conformance.Live{
		Address: os.Getenv("PTAH_YDB_TEST_URL"),
		Schemas: []string{directory},
		Changes: []conformance.Change{{
			Name:   "table",
			Apply:  conformance.Step{Units: []*protocol.Unit{{ChangeId: "create", Body: &protocol.Unit_Sql{Sql: "CREATE TABLE `" + table + "` (id Uint64 NOT NULL, PRIMARY KEY (id))"}}}},
			Revert: conformance.Step{Units: []*protocol.Unit{{ChangeId: "drop", Body: &protocol.Unit_Sql{Sql: "DROP TABLE `" + table + "`"}}}},
		}},
		Refused: []*protocol.Unit{{ChangeId: "missing", Body: &protocol.Unit_Sql{Sql: "DROP TABLE `" + path.Join(database, directory, "missing") + "`"}}},
	}

	report, err := conformance.Check(c.Context(), config)

	c.Assert(err, qt.IsNil)
	c.Assert(report.Err(), qt.IsNil)
}

// withCredential is address with a user whose password is the suite's
// canary. The test server knows no such user, so the provider either refuses
// the login or never sends it; either way nothing it returns may quote the
// password.
func withCredential(c *qt.C, address string) string {
	c.Helper()
	parsed, err := url.Parse(address)
	c.Assert(err, qt.IsNil)
	parsed.User = url.UserPassword("conformance", conformance.Canary)
	return parsed.String()
}
