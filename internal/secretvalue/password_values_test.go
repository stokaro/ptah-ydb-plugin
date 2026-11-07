package secretvalue_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/secretvalue"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A password that is JSON but not a hash, and one that starts with a brace
// and is no JSON, are sent as a PASSWORD, never as a HASH. Ported from the
// host's TestPasswordClause.
func TestExpandPasswordThatIsNoHash(t *testing.T) {
	for _, test := range []struct {
		name, password, want string
	}{
		{name: "JSON that is not a hash", password: `{"hash":"h"}`, want: `CREATE USER app PASSWORD '{"hash":"h"}';`},
		{name: "a brace that is not JSON", password: `{secret`, want: `CREATE USER app PASSWORD '{secret';`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := secretvalue.Expand("CREATE USER app PASSWORD $PTAH_SECRET_PASSWORD_APP;", environment(map[string]string{"PTAH_SECRET_PASSWORD_APP": test.password}))
			c.Assert(err, qt.IsNil)
			c.Assert(got.Text, qt.Equals, test.want)
		})
	}
}
