//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"os"
	"path"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"
)

// secretsSession opens a provider session on the test address, closed by
// the cleanup. The test is skipped on a line without secrets.
func secretsSession(c *qt.C, client *providerruntime.Client) *protocol.Session {
	c.Helper()
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: os.Getenv("PTAH_YDB_TEST_URL")})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), session.Id), qt.IsNil) })
	if !session.Capabilities[secrets.Capability] {
		c.Skip("the target does not support secrets")
	}
	return session
}

// secretEntry is one entry of a secret's access list as a read reports it.
type secretEntry struct {
	Role, Privilege string
	Implicit        bool
}

// secretEntries is every entry a read of directory reports on its secret
// name, for the roles given, sorted.
func secretEntries(c *qt.C, client *providerruntime.Client, session *protocol.Session, directory, name string, roles ...string) []secretEntry {
	c.Helper()
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.Id, Schemas: []string{directory}})
	c.Assert(err, qt.IsNil)
	var entries []secretEntry
	for _, grant := range snapshot.GetCommonObjects().GetGrants() {
		target := grant.GetTarget()
		if target.GetKind() == "secret" && target.GetSchema() == directory && target.GetName() == name && slices.Contains(roles, grant.GetRole()) {
			entries = append(entries, secretEntry{grant.GetRole(), grant.GetPrivilege(), grant.GetImplicit()})
		}
	}
	slices.SortFunc(entries, func(a, b secretEntry) int { return strings.Compare(a.Role+" "+a.Privilege, b.Role+" "+b.Privilege) })
	return entries
}

// Ported from the host's TestYDBObjectAccess_ASecretKeepsTheEntriesYDBGaveIt:
// a secret created in a directory a group may list gets an entry for that
// group from YDB, and one for the cluster's METADATA-READERS, which may list
// the database. No GRANT made either and the declaration names neither; the
// read reports both as implicit, so the declaration that created the secret
// plans nothing after. The group is the run's own and its LIST is granted
// outside the declaration, which declares grants without declaring roles.
func TestHostKeepsTheEntriesYDBGivesASecret_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	opened, directory, database := lockFixture(c)
	client := startProviderFor(c, provider.CommonObjectsFeature, provider.CommonAccessFeature)
	session := secretsSession(c, client)
	group := "ptahsecretg" + strings.ToLower(rand.Text())[:10]
	variable := "PTAH_SECRET_TEST_" + rand.Text()
	t.Setenv(variable, "synthetic-secret-"+rand.Text())
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "CREATE GROUP `"+group+"`"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// A missing secret is no failure; the directory goes with the fixture
		// and takes the group's entries with it.
		_ = execFixtureSchema(ctx, opened.DB, "DROP SECRET `"+path.Join(database, directory, "pw")+"`;")
		c.Check(execFixtureSchema(ctx, opened.DB, "DROP GROUP IF EXISTS `"+group+"`"), qt.IsNil)
	})
	c.Assert(execFixtureSchema(c.Context(), opened.DB, "GRANT LIST ON `"+path.Join(database, directory)+"` TO `"+group+"`"), qt.IsNil)
	source := "secrets:\n  pw:\n    schema: " + directory + "\n    value_env: " + variable + "\n" +
		"grants:\n  lister:\n    role: " + group + "\n    privileges: [LIST]\n    on_schema: " + directory + "\n"

	created, createdErr := hostApplySource(c, directory, "yaml", source)
	settled, settledErr := hostApplySource(c, directory, "yaml", source)

	c.Assert(createdErr, qt.IsNil, qt.Commentf("%s", created))
	c.Assert(created, qt.Matches, `(?s).*secret-operation.*Schema apply completed successfully.*`)
	c.Assert(settledErr, qt.IsNil, qt.Commentf("%s", settled))
	c.Assert(settled, qt.Contains, "Schema is synced, no changes to be made")
	c.Assert(secretEntries(c, client, session, directory, "pw", group, "METADATA-READERS"), qt.DeepEquals, []secretEntry{
		{Role: "METADATA-READERS", Privilege: "ydb.granular.describe_schema", Implicit: true},
		{Role: group, Privilege: "ydb.granular.describe_schema", Implicit: true},
	})
}
