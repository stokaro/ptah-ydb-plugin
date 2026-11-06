//go:build integration

package integration_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A secret declaration the provider refuses while it decodes the source says
// why, as the built-in engine at host 74de0b65b did on 25.1.4.7 and 26.2.1.14:
// the secret, the attribute and what it takes. The provider printed the code
// alone, `provider decode source failed (InvalidArgument)` (gap G144). The
// reason leaves out the variable the declaration names, which the built-in
// quoted, because a reason never quotes a value a declaration wrote; a YAML
// secret is named by its key, and the host names a Go directive's place.
func TestHostSaysWhyItRefusesASecretDeclaration_FailurePath(t *testing.T) {
	tests := []struct {
		name, format, text, want string
	}{
		{name: "YAML", format: "yaml", text: "secrets:\n  pw:\n    schema: {dir}\n    value_env: HOME_G144\n",
			want: `provider decode source failed (InvalidArgument): secret "pw": invalid value_env: ` +
				`the variable it names does not start with PTAH_SECRET_ and a name after it; Ptah reads a secret's value only from a variable under that prefix`},
		{name: "Go", format: "go", text: "package entities\n\n//ptah:schema:secret name=\"pw\" schema=\"{dir}\" value_env=\"HOME_G144\"\ntype Refused struct{}\n",
			want: `provider decode source failed (InvalidArgument): invalid value_env: ` +
				`the variable it names does not start with PTAH_SECRET_ and a name after it; Ptah reads a secret's value only from a variable under that prefix`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			_, directory, _ := inspectionFixture(c)

			output, err := hostApplySource(c, directory, test.format, strings.ReplaceAll(test.text, "{dir}", directory))

			c.Assert(err, qt.IsNotNil)
			c.Assert(output, qt.Contains, test.want)
			c.Assert(output, qt.Not(qt.Contains), "HOME_G144")
		})
	}
}
