package inspect

// White-box testing required: a row table reaches the reservation only inside
// ReadObjects, which opens a table session on a YDB driver and describes the
// table through it, so no offline caller reaches the decision. The tests
// classify the description with the function the read uses and hand its
// purpose to the decision. The host-driven test
// TestHostRefusesADeclarationOfTheRepeatableRecord_FailurePath drives the
// public read against a server.
import (
	"path"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// repeatableRecord is the native description of the record of repeatable
// migrations as the provider creates it under name.
func repeatableRecord(name string) *native.DescribeTableResult {
	column := func(name string, id Ydb.Type_PrimitiveTypeId) *native.ColumnMeta {
		return &native.ColumnMeta{Name: name, Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: id}}, NotNull: new(true)}
	}
	return &native.DescribeTableResult{
		Self:       &Ydb_Scheme.Entry{Name: name, Type: Ydb_Scheme.Entry_TABLE},
		Columns:    []*native.ColumnMeta{column("identity", Ydb.Type_UTF8), column("row", Ydb.Type_STRING), column("body", Ydb.Type_STRING)},
		PrimaryKey: []string{"identity"},
		Attributes: map[string]string{"ptah.repeatable": "stokaro/ptah-ydb-plugin/repeatable-v1"},
		StoreType:  Ydb_Table.StoreType_STORE_TYPE_ROW,
	}
}

// reservedNames names each reservation of a read as its purpose, its
// directory and its name.
func reservedNames(result *protocol.Schema) []string {
	var names []string
	for _, reservation := range result.GetReservedObjects() {
		names = append(names, reservation.GetPurpose().String()+" "+path.Join(reservation.GetObject().GetSchema(), reservation.GetObject().GetName()))
	}
	return names
}

// The record of repeatable migrations is reserved storage of purpose
// REPEATABLE for a host that negotiated reading it as reserved storage. A host
// that did not is told of it as an unread table when it takes unread tables,
// and of nothing otherwise. In no case does the read go on to read it as a
// table of the schema, which a plan would drop as undeclared.
func TestRowTableReadOfTheRepeatableRecord(t *testing.T) {
	for _, test := range []struct {
		name     string
		common   Common
		reserved []string
		unread   []string
	}{
		{name: "a host that reads the record as reserved storage", common: Common{RepeatableRecord: true, UnreadTables: true},
			reserved: []string{"REPEATABLE app/revisions_repeatable"}},
		{name: "a host that takes unread tables", common: Common{UnreadTables: true},
			unread: []string{"app/revisions_repeatable: app/revisions_repeatable is the record of repeatable migrations, which this host does not read as reserved storage"}},
		{name: "a host that takes neither"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			described, err := classifyTable("/local/app/revisions_repeatable", repeatableRecord("revisions_repeatable"))
			c.Assert(err, qt.IsNil)
			read := nestedTraversal(nil)
			read.common = test.common

			c.Assert(read.reserved("app", "revisions_repeatable", described.reservation), qt.IsTrue)
			c.Assert(read.unreadTables(c.Context(), nil), qt.IsNil)

			c.Assert(reservedNames(read.result), qt.DeepEquals, test.reserved)
			c.Assert(unreadNames(read.result), qt.DeepEquals, test.unread)
		})
	}
}

// A table of the record's layout without its marker is no reserved storage:
// the read records nothing for it and goes on to read it as a table of the
// schema.
func TestRowTableReadOfAnUnmarkedTable(t *testing.T) {
	c := testcheck.New(t)
	value := repeatableRecord("revisions_repeatable")
	value.Attributes = nil
	described, err := classifyTable("/local/app/revisions_repeatable", value)
	c.Assert(err, qt.IsNil)
	read := nestedTraversal(nil)
	read.common = Common{RepeatableRecord: true, UnreadTables: true}

	c.Assert(read.reserved("app", "revisions_repeatable", described.reservation), qt.IsFalse)
	c.Assert(read.unreadTables(c.Context(), nil), qt.IsNil)

	c.Assert(read.result.ReservedObjects, qt.HasLen, 0)
	c.Assert(read.result.UnreadTables, qt.HasLen, 0)
}
