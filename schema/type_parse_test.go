package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/proto"
)

func TestStorageTypeRoundTrip(t *testing.T) {
	for _, spelling := range []string{"Bool", "Int8", "Uint8", "Int16", "Uint16", "Int32", "Uint32", "Int64", "Uint64",
		"Float", "Double", "Date", "Datetime", "Timestamp", "Interval", "Date32", "Datetime64", "Timestamp64", "Interval64",
		"String", "Utf8", "Yson", "Json", "Uuid", "JsonDocument", "DyNumber", "Decimal(35,0)", "Decimal(22,9)"} {
		for _, nullable := range []bool{false, true} {
			t.Run(spelling+map[bool]string{false: "", true: " nullable"}[nullable], func(t *testing.T) {
				c := qt.New(t)
				native, err := schema.StorageType(spelling, nullable)
				c.Assert(err, qt.IsNil)
				actual, optional, err := schema.ColumnType(native)
				c.Assert(err, qt.IsNil)
				c.Assert(actual, qt.Equals, spelling)
				c.Assert(optional, qt.Equals, nullable)
			})
		}
	}
}

func TestStorageTypeGrammar(t *testing.T) {
	c := qt.New(t)
	result, err := schema.StorageType("uInT64", false)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(result, primitive(Ydb.Type_UINT64)), qt.IsTrue)
	result, err = schema.StorageType("decimal( 22 , 9 )", true)
	c.Assert(err, qt.IsNil)
	name, nullable, err := schema.ColumnType(result)
	c.Assert(err, qt.IsNil)
	c.Assert(name, qt.Equals, "Decimal(22,9)")
	c.Assert(nullable, qt.IsTrue)
	for _, spelling := range []string{"", "Uint64?", "Optional<Uint64>", " Uint64", "Uint64 ", "Uint64; DROP TABLE x",
		"List<Uint64>", "Decimal", "Decimal(0,0)", "Decimal(36,0)", "Decimal(2,3)", "Decimal(2)", "Decimal(2,1,0)",
		"Decimal(+2,1)", "Decimal(2,-1)", "Decimal(4294967296,0)", "Decimal(2,1e0)", "Decimal(2,1) trailing"} {
		t.Run(spelling, func(t *testing.T) {
			c := qt.New(t)
			value, err := schema.StorageType(spelling, false)
			c.Assert(err, qt.IsNotNil)
			c.Assert(value, qt.IsNil)
		})
	}
}
