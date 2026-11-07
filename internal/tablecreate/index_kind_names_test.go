package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// An index type YDB has no method for is refused by name, in any case:
// PostgreSQL's GIN, a bare LOCAL, which names no local method, a json
// method, and pgvector's IVFFLAT, which is pointed at YDB's vector index.
//
// Ported from the host's TestKindOf_FailurePath.
func TestCompileDeclaredIndexes_RefusesIndexKindsYDBLacks(t *testing.T) {
	for _, test := range []struct{ name, method, wantErr string }{
		{"PostgreSQL's GIN", "GIN", `.*index "i": index method "GIN" has no YDB counterpart: a row table has global indexes, .*`},
		{"a bare LOCAL", "LOCAL", `.*index "i": index method "LOCAL" has no YDB counterpart: a row table has global indexes, .*`},
		{"a json method", "json", `.*index "i": index method "json" has no YDB counterpart: a row table has global indexes, .*`},
		{"pgvector's IVFFLAT", "IVFFLAT", `.*index "i": index method "IVFFLAT" is pgvector's and has no YDB counterpart: YDB's vector index is vector_kmeans_tree, .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request, err := tablecreate.Compile(owner(), indexedTable(&protocol.DeclaredIndex{Name: "i", Fields: []string{"mail"}, Type: test.method}), nil)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(request, qt.IsNil)
		})
	}
}
