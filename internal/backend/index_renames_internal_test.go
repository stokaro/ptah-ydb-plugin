package backend

// White-box testing required: a session's capabilities are assembled while it
// opens against a live YDB server, and no exported call returns them without
// one. The live tests under integration/ cover an opened session.

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A session reports index_rename as EnableMoveIndex decides it to a host
// that lists planning.index-renames, and false to any other host, which has
// no step to send a renamed index with.
func TestIndexRenamesFollowTheHost_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		host  []string
		moves bool
		want  bool
	}{
		{"a host that renames, the flag on", []string{provider.IndexRenamesFeature}, true, true},
		{"a host that renames, the flag off", []string{provider.IndexRenamesFeature}, false, false},
		{"a host that does not rename", []string{provider.ObservedIndexesFeature}, true, false},
		{"a host that lists nothing", nil, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			held := map[string]bool{"index_rename": test.moves, "secrets": true}
			got := indexRenames(provider.ContextWithHostFeatures(context.Background(), test.host), held)
			c.Assert(got, qt.DeepEquals, map[string]bool{"index_rename": test.want, "secrets": true})
			c.Assert(held["index_rename"], qt.Equals, test.moves)
		})
	}
}
