//go:build integration

package integration_test

import (
	"os"
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// Ported from the host's TestYDBBinary_ReplaysASecretInADevRealm: `migrations
// validate` replays a directory that creates and rotates a secret on a dev
// realm in the test database. The secret is a path the realm confines, the
// replay approves the input each statement reads and takes its value from the
// environment as an apply does, and the realm is gone, with the secret in it,
// when the host exits. No output holds the value. The refusal of the
// deprecated secret object is TestHostRefusesTheDeprecatedSecretObjectInAReplay_FailurePath.
func TestHostReplaysASecretInADevRealm_HappyPath(t *testing.T) {
	value := secretValue()
	variable := secretVariable(t, value)
	c := testcheck.New(t)
	opened, directory, database := secretsFixture(c)
	secret := path.Join(directory, "pw")
	migrations := writeFiles(c, c.TempDir(), map[string]string{ // #nosec G101 -- statements that name a variable, not a credential
		"0000000001_secret.up.sql": "CREATE SECRET `" + secret + "` WITH (value = $" + variable + ");\n" +
			"ALTER SECRET `" + secret + "` WITH (value = $" + variable + ");\n",
		"0000000001_secret.down.sql": "DROP SECRET `" + secret + "`;\n",
	})
	hashed, hashErr := hostOffline(c, "migrations", "hash", "--dir", migrations)
	c.Assert(hashErr, qt.IsNil, qt.Commentf("%s", hashed))

	validated, err := hostCommand(c, "migrations", "validate", "--dir", migrations, "--dev-url", os.Getenv("PTAH_YDB_TEST_URL"))

	c.Assert(err, qt.IsNil, qt.Commentf("%s", validated))
	c.Assert(validated, qt.Contains, "OK: migration SQL validated on dev database")
	c.Assert(validated, qt.Not(qt.Contains), value)
	c.Assert(heldEntries(c, opened, path.Join(database, directory)), qt.HasLen, 0)
	c.Assert(entryNames(heldEntries(c, opened, database)), qt.Not(qt.Contains), connection.RealmDirectory)
}
