//go:build integration

package integration_test

import (
	"context"
	"net/url"
	"os"
	"path"
	"slices"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// realmsDirectory makes the directory the dev realms of the test database
// live in, which other runs share, and returns its path. The cleanup removes
// it again when this run made it and nothing is left in it.
func realmsDirectory(c *qt.C, admin *connection.Connection, database string) string {
	c.Helper()
	realms := path.Join(database, connection.RealmDirectory)
	existed := slices.Contains(childNames(c, admin, database), connection.RealmDirectory)
	c.Assert(admin.Driver.Scheme().MakeDirectory(c.Context(), realms), qt.IsNil)
	c.Cleanup(func() {
		if existed {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// A directory another run's realm went into in the meantime is
		// refused, and stays.
		_ = admin.Driver.Scheme().RemoveDirectory(ctx, realms)
	})
	return realms
}

// grantOn grants the user name rights on the directory at absolute, and the
// cleanup revokes every right of the user there, while the directory exists.
func grantOn(c *qt.C, admin *connection.Connection, absolute, name, rights string) {
	c.Helper()
	c.Assert(execFixtureSchema(c.Context(), admin.DB, "GRANT "+rights+" ON `"+absolute+"` TO `"+name+"`"), qt.IsNil)
	c.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// A removed directory took the user's rights along.
		_ = execFixtureSchema(ctx, admin.DB, "REVOKE ALL ON `"+absolute+"` FROM `"+name+"`")
	})
}

// provision asks the provider for a dev realm in the database address names.
// The cleanup releases a realm the reply holds, so none is left behind
// whatever the test asserts; a lease the test released already is refused
// again, and that is not an error.
func provision(c *qt.C, client *providerruntime.Client, address string) (*protocol.DevDatabase, error) {
	c.Helper()
	reply, err := client.ProvisionDevDatabase(c.Context(), &protocol.ProvisionDevRequest{Engine: "ydb", Address: address})
	c.Cleanup(func() {
		if reply.GetLease() == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = client.ReleaseDevDatabase(ctx, &protocol.ReleaseDevRequest{Engine: "ydb", Lease: reply.GetLease()})
	})
	return reply, err
}

// realmName is the name of the realm a dev realm address names.
func realmName(c *qt.C, address string) string {
	c.Helper()
	parsed, err := url.Parse(address)
	c.Assert(err, qt.IsNil)
	return parsed.Query().Get("dev_realm")
}

// Ported from the host's TestYDBDevRealm_NamesTheRightItNeeds: a realm is a
// directory, which the scheme service creates only for a user who holds
// ydb.granular.create_directory. A user who may create tables, describe and
// remove in the realms' directory is refused a realm with the right named, and
// gets one once granted it. The rights are granted on the realms' directory,
// so the database root's access list stays as it is.
func TestDevRealmNamesTheRightItNeedsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	base, err := connection.Parse(os.Getenv("PTAH_YDB_TEST_URL"))
	c.Assert(err, qt.IsNil)
	admin := openAs(c, os.Getenv("PTAH_YDB_TEST_URL"))
	name, address := testUser(c, admin, "ptahrealmu")
	realms := realmsDirectory(c, admin, base.Database())
	grantOn(c, admin, realms, name, "'ydb.granular.create_table', 'ydb.granular.describe_schema', 'ydb.granular.remove_schema'")
	client := startProviderFor(c, provider.DevRealmsFeature)

	refused, refusedErr := provision(c, client, address)
	grantOn(c, admin, realms, name, "'ydb.granular.create_directory'")
	created, createdErr := provision(c, client, address)
	c.Assert(createdErr, qt.IsNil)
	c.Assert(created.GetRefusal(), qt.IsNil)
	held := childNames(c, admin, realms)
	_, releaseErr := client.ReleaseDevDatabase(c.Context(), &protocol.ReleaseDevRequest{Engine: "ydb", Lease: created.GetLease()})

	c.Assert(refusedErr, qt.IsNil)
	c.Assert(refused.GetRefusal().GetKind(), qt.Equals, protocol.DevDatabaseRefusal_UNAVAILABLE)
	c.Assert(refused.GetRefusal().GetReason(), qt.Equals, "the database the dev realm address names could not take a realm: "+
		"the realm's directory could not be created (UNAUTHORIZED); creating a directory needs the ydb.granular.create_directory right on the database")
	c.Assert(held, qt.Contains, realmName(c, created.GetUrl()))
	c.Assert(releaseErr, qt.IsNil)
	c.Assert(childNames(c, admin, realms), qt.Not(qt.Contains), realmName(c, created.GetUrl()))
}
