package ydbacl_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
)

// TestDescribe pins how a refusal names the object a grant is about: the
// database, a directory, and any other scheme object by its kind, however the
// kind is cased or spaced. Ported from the host's TestDescribe.
func TestDescribe(t *testing.T) {
	for _, test := range []struct {
		kind, name, want string
	}{
		{kind: "DATABASE", want: "the database"},
		{kind: "schema", name: "shop/eu", want: "directory shop/eu"},
		{kind: "TABLE", name: "shop/orders", want: "table shop/orders"},
		{kind: "external  table", name: "ext/events", want: "external table ext/events"},
	} {
		t.Run(test.want, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbacl.Describe(test.kind, test.name), qt.Equals, test.want)
		})
	}
}
