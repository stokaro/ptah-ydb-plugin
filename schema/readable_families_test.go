package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// describedFamilies is a table description holding the families given.
func describedFamilies(families ...*native.ColumnFamily) *native.DescribeTableResult {
	return &native.DescribeTableResult{PrimaryKey: []string{"id"}, ColumnFamilies: families}
}

// unknownFieldFamily is a family as a newer server could describe it, with a
// field the pinned protocol does not know, number 99.
func unknownFieldFamily() *native.ColumnFamily {
	family := &native.ColumnFamily{Name: "cold", Compression: Ydb_Table.ColumnFamily_COMPRESSION_LZ4}
	family.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
	return family
}

// unknownCompressionFamily is a family as a newer server could describe it,
// with a compression value the pinned protocol has no name for.
func unknownCompressionFamily() *native.ColumnFamily {
	return &native.ColumnFamily{Name: "cold", Compression: 42}
}

// A family that holds what the model does not know leaves the description
// without its families, with the reason the read records the table's
// properties unrepresentable for, and the description given is not changed.
// Ported from the read half of the host's
// TestReader_RecordsColumnFamiliesItDoesNotRead.
func TestReadableFamilies_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, want string
		family     *native.ColumnFamily
	}{
		{name: "a field the protocol does not know", family: unknownFieldFamily(),
			want: `column family "cold" holds what this provider does not read (unknown fields in ptah.ydb.native.ColumnFamily)`},
		{name: "an enum value the protocol does not know", family: unknownCompressionFamily(),
			want: `column family "cold" holds what this provider does not read (unknown enum in ptah.ydb.native.ColumnFamily.compression)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			description := describedFamilies(&native.ColumnFamily{Name: "default"}, test.family)
			original := proto.CloneOf(description)

			readable, reason := schema.ReadableFamilies(description)

			c.Assert(reason, qt.Equals, test.want)
			c.Assert(readable.GetColumnFamilies(), qt.HasLen, 0)
			c.Assert(readable.GetPrimaryKey(), qt.DeepEquals, []string{"id"})
			c.Assert(proto.Equal(description, original), qt.IsTrue)
		})
	}
}

// Families the model knows are read as they are.
func TestReadableFamilies_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	description := describedFamilies(&native.ColumnFamily{Name: "default"}, &native.ColumnFamily{Name: "cold", Compression: Ydb_Table.ColumnFamily_COMPRESSION_LZ4})

	readable, reason := schema.ReadableFamilies(description)

	c.Assert(reason, qt.Equals, "")
	c.Assert(readable, qt.Equals, description)
}
