package backend_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/secretsource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// A secret declaration DecodeSource refuses gives the reason a host prints, in
// the words of the host's embedded engine: the secret's key, the attribute and
// what it takes (gap G144). It never quotes the value a declaration wrote or
// the variable it names, which the host's engine quoted; a name it quotes is
// the secret's own. A key too long for a reason a host shows is left out.
// The empty name, the unclean directory and the trailing slash are ported
// from the host's ydbsecret TestDeclare_FailurePath and
// TestValidateIdentity_FailurePath.
func TestSecretSourceRefusalGivesTheReason_FailurePath(t *testing.T) {
	// #nosec G101 -- the rows are declarations and the reasons a host prints for them, with made-up variable names, not credentials
	tests := []struct {
		name, secrets, reason string
	}{
		{name: "no variable", secrets: `{"pw":{}}`,
			reason: `secret "pw": invalid value_env: a secret names the environment variable that holds its value`},
		{name: "a variable outside the prefix", secrets: `{"pw":{"value_env":"DO_NOT_REPORT"}}`,
			reason: `secret "pw": invalid value_env: the variable it names does not start with PTAH_SECRET_ and a name after it; ` +
				`Ptah reads a secret's value only from a variable under that prefix, so a migration file cannot copy any other variable of the machine that applies it`},
		{name: "a variable with another character", secrets: `{"pw":{"value_env":"PTAH_SECRET_DO-NOT-REPORT"}}`,
			reason: `secret "pw": invalid value_env: the variable it names holds a character other than an ASCII letter, a digit or an underscore`},
		{name: "a literal value", secrets: `{"pw":{"value":"DO_NOT_REPORT","value_env":"PTAH_SECRET_PW"}}`,
			reason: `secret "pw": invalid value: a secret's value is never written in a schema file; name the environment variable that holds it with value_env`},
		{name: "a path in the name", secrets: `{"pw":{"name":"ext/pw","value_env":"PTAH_SECRET_PW"}}`,
			reason: `secret "pw": invalid name: "ext/pw" holds a slash; name the directory with schema`},
		{name: "a name that is not a path segment", secrets: `{"pw":{"name":"..","value_env":"PTAH_SECRET_PW"}}`,
			reason: `secret "pw": invalid name: ".." is not a path segment`},
		{name: "an empty name", secrets: `{"pw":{"name":" ","value_env":"PTAH_SECRET_PW"}}`,
			reason: `secret "pw": invalid name: a secret needs a name`},
		{name: "an unclean directory", secrets: `{"pw":{"schema":"ext//aws","value_env":"PTAH_SECRET_PW"}}`,
			reason: `secret "pw": invalid schema: "ext//aws" is not a directory path relative to the database root`},
		{name: "a directory with a trailing slash", secrets: `{"pw":{"schema":"ext/","value_env":"PTAH_SECRET_PW"}}`,
			reason: `secret "pw": invalid schema: "ext/" is not a directory path relative to the database root`},
		{name: "a directory written from the server root", secrets: `{"pw":{"schema":"/local/ext","value_env":"PTAH_SECRET_PW"}}`,
			reason: `secret "pw": invalid schema: "/local/ext" starts with a slash; name the directory relative to the database root, without the database's own path`},
		{name: "a directory outside the database", secrets: `{"pw":{"schema":"../ext","value_env":"PTAH_SECRET_PW"}}`,
			reason: `secret "pw": invalid schema: "../ext" is not a directory path relative to the database root`},
		{name: "one path declared twice", secrets: `{"a":{"name":"pw","value_env":"PTAH_SECRET_PW"},"b":{"name":"pw","value_env":"PTAH_SECRET_PW"}}`,
			reason: `secret "b": secret "pw" is declared twice`},
		{name: "an unknown attribute", secrets: `{"pw":{"value_env":"PTAH_SECRET_PW","flavor":"DO_NOT_REPORT"}}`,
			reason: `secret "pw": unknown attribute "flavor"; a secret takes name, schema and value_env`},
		{name: "an attribute that is not text", secrets: `{"pw":{"value_env":null}}`,
			reason: `secret "pw": invalid value_env: it takes text`},
		{name: "an entry that is not a mapping", secrets: `{"pw":"DO_NOT_REPORT"}`,
			reason: `secret "pw": a secret is declared as a mapping of name, schema and value_env`},
		{name: "a key too long to repeat", secrets: `{"` + strings.Repeat("k", 600) + `":{}}`,
			reason: `invalid value_env: a secret names the environment variable that holds its value`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := testsource.YAMLDocument(`{"secrets":`+test.secrets+`}`, secretsource.YAMLKey(), schema.NativeDesiredModel(schema.SecretProperties))
			c.Assert(err, qt.IsNil)

			result, err := backend.New().DecodeSource(c.Context(), request)

			c.Assert(result, qt.IsNil)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(statusReasons(c, err), qt.DeepEquals, []string{test.reason})
		})
	}
}
