package secrets_test

import (
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A variable under the prefix, spelled as YQL spells a named expression, is
// one a secret's value may come from.
func TestCheckValueEnv_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(secrets.CheckValueEnv("PTAH_SECRET_PG_PASSWORD"), qt.IsNil)
}

// outsidePrefix is why a variable outside the prefix is refused.
const outsidePrefix = "does not start with PTAH_SECRET_ and a name after it; Ptah reads a secret's value only from a variable " +
	"under that prefix, so a migration file cannot copy any other variable of the machine that applies it"

// CheckValueEnv's error names the variable, as the host's embedded engine
// did, and its Reason does not, so a reason the provider gives a host may
// carry it.
func TestCheckValueEnv_FailurePath(t *testing.T) {
	tests := []struct {
		name, variable, err, reason string
	}{
		{name: "no variable", variable: "",
			err:    "invalid value_env: a secret names the environment variable that holds its value",
			reason: "a secret names the environment variable that holds its value"},
		{name: "outside the prefix", variable: "HOME", err: `invalid value_env: "HOME" ` + outsidePrefix, reason: outsidePrefix},
		{name: "the prefix alone", variable: "PTAH_SECRET_", err: `invalid value_env: "PTAH_SECRET_" ` + outsidePrefix, reason: outsidePrefix},
		{name: "another character", variable: "PTAH_SECRET_PG-PASSWORD",
			err:    `invalid value_env: "PTAH_SECRET_PG-PASSWORD" holds a character other than an ASCII letter, a digit or an underscore`,
			reason: "holds a character other than an ASCII letter, a digit or an underscore"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			err := secrets.CheckValueEnv(test.variable)

			c.Assert(err, qt.IsNotNil)
			c.Assert(err.Error(), qt.Equals, test.err)
			declared, ok := errors.AsType[*secrets.DeclarationError](err)
			c.Assert(ok, qt.IsTrue)
			c.Assert(declared.Reason, qt.Equals, test.reason)
			c.Assert(declared.Variable, qt.Equals, test.variable)
		})
	}
}

// The variable Ptah names for a secret it did not declare is the prefix and
// the secret's path in upper case, each character a variable cannot hold an
// underscore, and it passes CheckValueEnv. Ported from the host's
// TestDefaultValueEnv_HappyPath.
func TestDefaultValueEnv_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, schema, secret, want string
	}{
		{name: "at the root", secret: "pw", want: "PTAH_SECRET_PW"},
		{name: "in a directory, with characters a variable cannot hold", schema: "/app/ext/", secret: "pg.pass-1", want: "PTAH_SECRET_APP_EXT_PG_PASS_1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got := secrets.DefaultValueEnv(test.schema, test.secret)
			c.Assert(got, qt.Equals, test.want)
			c.Assert(secrets.CheckValueEnv(got), qt.IsNil)
		})
	}
}
