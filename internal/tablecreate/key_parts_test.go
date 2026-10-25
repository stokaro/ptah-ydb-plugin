package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// keyParts writes columns as the key parts a SQL schema reading declares.
func keyParts(columns ...string) []*protocol.DeclaredKeyPart {
	parts := make([]*protocol.DeclaredKeyPart, 0, len(columns))
	for _, column := range columns {
		parts = append(parts, &protocol.DeclaredKeyPart{Column: column})
	}
	return parts
}

// unfaceted is the fixture table without its facets, so its key comes only
// from the key, the key parts and the column flags, where both key columns
// are flagged and the flags alone cannot order them.
func unfaceted() *protocol.DeclaredTable {
	table := declaration()
	table.Features = nil
	return table
}

// A table a SQL schema reading declares states its key as key parts too. The
// parts order the key where nothing else does, and parts that repeat the key
// are the key.
func TestCompileKeyParts_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   []string
		parts []*protocol.DeclaredKeyPart
		want  []string
	}{
		{name: "parts alone", parts: keyParts("second", "first"), want: []string{"second", "first"}},
		{name: "parts in the other order", parts: keyParts("first", "second"), want: []string{"first", "second"}},
		{name: "parts that repeat the key", key: []string{"second", "first"}, parts: keyParts("second", "first"), want: []string{"second", "first"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			table := unfaceted()
			table.PrimaryKey, table.PrimaryKeyParts = test.key, test.parts
			result, err := tablecreate.Compile(owner(), table)
			c.Assert(err, qt.IsNil)
			c.Assert(result.PrimaryKey, qt.DeepEquals, test.want)
		})
	}
}

// Key parts YDB has no spelling for, and parts that name another key than
// the key does, are refused before anything is created.
func TestCompileKeyParts_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   []string
		parts []*protocol.DeclaredKeyPart
		want  string
	}{
		{name: "parts in another order than the key", key: []string{"second", "first"}, parts: keyParts("first", "second"),
			want: "the key parts of a YDB table declaration disagree with its key"},
		{name: "parts of another key", key: []string{"second", "first"}, parts: keyParts("second"),
			want: "the key parts of a YDB table declaration disagree with its key"},
		{name: "a descending part", parts: []*protocol.DeclaredKeyPart{{Column: "second", Descending: true}, {Column: "first"}},
			want: `.*YDB table creation does not support .*descending.*`},
		{name: "a prefix part", parts: []*protocol.DeclaredKeyPart{{Column: "second", Prefix: "4"}, {Column: "first"}},
			want: `.*YDB table creation does not support .*prefix.*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			table := unfaceted()
			table.PrimaryKey, table.PrimaryKeyParts = test.key, test.parts
			result, err := tablecreate.Compile(owner(), table)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(result, qt.IsNil)
		})
	}
}
