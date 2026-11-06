package backend

// White-box testing required: that the provider checks its requests against
// one description built once, rather than one built for each check, is not
// observable through an exported call; only the time the checks take shows
// it. The exported Description returns a copy, which
// TestDescriptionReturnsACopyTheCallerMayChange covers.

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// TestDescriptionIsBuiltOnce pins that every check reads the same
// description, and that it equals a freshly built one.
func TestDescriptionIsBuiltOnce(t *testing.T) {
	c := testcheck.New(t)

	first, second := described(), described()

	c.Assert(first, qt.Equals, second)
	c.Assert(proto.Equal(first, buildDescription()), qt.IsTrue)
}
