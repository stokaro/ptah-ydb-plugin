package replication_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	ydbreplication "github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// SecretPaths names each secret a connection names by path once, relative to
// the database: a relative path as written, an absolute path under the
// database as the relative path it holds. A path written absolute where the
// database is not known, and one that cannot name a secret, name none, as the
// host's embedded engine read them.
func TestSecretPaths_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name       string
		root       string
		connection ydbreplication.Connection
		want       []ydbdecl.Path
	}{
		{name: "a relative token path", root: "/local", connection: ydbreplication.Connection{TokenSecretPath: "secrets/token"},
			want: []ydbdecl.Path{{Schema: "secrets", Name: "token"}}},
		{name: "a password path at the root", root: "/local", connection: ydbreplication.Connection{User: "reader", PasswordSecretPath: "password"},
			want: []ydbdecl.Path{{Name: "password"}}},
		{name: "an absolute path under the database", root: "/local/", connection: ydbreplication.Connection{TokenSecretPath: "/local/secrets/token"},
			want: []ydbdecl.Path{{Schema: "secrets", Name: "token"}}},
		{name: "one secret named twice", root: "/local", connection: ydbreplication.Connection{TokenSecretPath: "secrets/token", PasswordSecretPath: "/local/secrets/token"},
			want: []ydbdecl.Path{{Schema: "secrets", Name: "token"}}},
		{name: "an absolute path where the database is not known", connection: ydbreplication.Connection{TokenSecretPath: "/local/secrets/token"}},
		{name: "a path that names no secret", root: "/local", connection: ydbreplication.Connection{TokenSecretPath: "secrets/../token"}},
		{name: "a secret named by name", root: "/local", connection: ydbreplication.Connection{TokenSecretName: "token"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			paths, err := ydbreplication.SecretPaths(test.root, test.connection)

			c.Assert(err, qt.IsNil)
			c.Assert(paths, qt.DeepEquals, test.want)
		})
	}
}

// A path outside the database is refused in the words of the host's embedded
// engine: no statement of the replication or transfer can read the secret.
func TestSecretPaths_FailurePath(t *testing.T) {
	c := testcheck.New(t)

	paths, err := ydbreplication.SecretPaths("/local", ydbreplication.Connection{TokenSecretPath: "/other/secrets/token"})

	c.Assert(err, qt.ErrorMatches, `secret path "/other/secrets/token" is outside the database /local, so no statement of it can read the secret`)
	c.Assert(paths, qt.IsNil)
}
