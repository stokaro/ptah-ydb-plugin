package tablecreate

// White-box testing required: Compile requires a primary key, so the former
// host's no-key matching case cannot reach the folding decision through a
// complete table declaration. The related index naming cases live here too.

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// These cases port the old host's TestUniqueIndexName and TestUniqueIsTheKey.
func TestUniqueIndexName(t *testing.T) {
	for _, test := range []struct {
		name, table, declared, want string
		columns                     []string
	}{
		{"named constraint", "users", "uq_users_email", "uq_users_email", []string{"email"}},
		{"column", "users", "", "users_email_key", []string{"email"}},
		{"unnamed composite constraint", "users", "  ", "users_tenant_login_key", []string{"tenant", "login"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(uniqueIndexName(test.table, test.declared, test.columns), qt.Equals, test.want)
		})
	}
}

func TestUniqueIsTheKey(t *testing.T) {
	for _, test := range []struct {
		name    string
		columns []string
		key     []string
		want    bool
	}{
		{"the key", []string{"id"}, []string{"id"}, true},
		{"the key in another order", []string{"b", "a"}, []string{"a", "b"}, true},
		{"part of the key", []string{"a"}, []string{"a", "b"}, false},
		{"the key and more", []string{"a", "b"}, []string{"a"}, false},
		{"table without a key", []string{"a"}, nil, false},
		{"repeated column", []string{"a", "a"}, []string{"a", "b"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(uniqueIsKey(test.columns, test.key), qt.Equals, test.want)
		})
	}
}
