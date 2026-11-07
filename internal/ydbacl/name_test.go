package ydbacl_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
)

// TestCheckName_HappyPath pins the names YDB takes for a user or a group:
// lower-case letters and digits, a leading digit and a long name included.
// Ported from the host's TestCheckName_HappyPath.
func TestCheckName_HappyPath(t *testing.T) {
	for _, name := range []string{"app", "9app", "a1b2", strings.Repeat("a", 88)} {
		t.Run(name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbacl.CheckName(name), qt.IsNil)
		})
	}
}

// TestCheckName_FailurePath pins the empty name and the characters YDB
// refuses with `Name is not allowed`, measured by the host on 25.1.4.7 to
// 26.2.1.14; the refusal names the first offending character. Ported from
// the host's TestCheckName_FailurePath.
func TestCheckName_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		wantErr string
	}{
		{name: "", wantErr: `YDB takes a user or group name of lower-case ASCII letters and digits only, and the name is empty`},
		{name: "app_user", wantErr: `YDB takes a user or group name of lower-case ASCII letters and digits only: "app_user" holds '_'`},
		{name: "app-user", wantErr: `.*: "app-user" holds '-'`},
		{name: "app.user", wantErr: `.*: "app.user" holds '\.'`},
		{name: "app@corp", wantErr: `.*: "app@corp" holds '@'`},
		{name: "App", wantErr: `.*: "App" holds 'A'`},
		{name: "DATA-READERS", wantErr: `.*: "DATA-READERS" holds 'D'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbacl.CheckName(tc.name), qt.ErrorMatches, tc.wantErr)
		})
	}
}
