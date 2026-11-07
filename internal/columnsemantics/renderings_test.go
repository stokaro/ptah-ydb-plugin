package columnsemantics_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A declared type compares equal to each YDB type it may have been created
// as, so a table built on 25.1 and read on 26.2 compares equal to the
// declaration that built it: TIMESTAMP, DATE and INTERVAL are either width.
// A YDB narrow type is one type only, a declaration with no YDB counterpart
// equals none, and a declared vector is the String its column holds. Ported
// from the host's TestRenderings.
func TestColumnTypeComparesEveryRendering(t *testing.T) {
	same, changed := protocol.ColumnDifference_SAME, protocol.ColumnDifference_CHANGED
	for _, tc := range []struct {
		name              string
		declared, catalog string
		relation          protocol.ColumnDifference_Relation
		current, desired  string
	}{
		{"a wide timestamp", "TIMESTAMP", "Timestamp64", same, "Timestamp64", "Timestamp64"},
		{"a narrow timestamp", "TIMESTAMP", "Timestamp", same, "Timestamp", "Timestamp"},
		{"a wide date", "DATE", "Date32", same, "Date32", "Date32"},
		{"a narrow date", "DATE", "Date", same, "Date", "Date"},
		{"a wide interval", "INTERVAL", "Interval64", same, "Interval64", "Interval64"},
		{"a narrow interval", "INTERVAL", "Interval", same, "Interval", "Interval"},
		{"a string keeps no length", "VARCHAR(100)", "Utf8", same, "Utf8", "Utf8"},
		{"a narrow native against itself", "Timestamp", "Timestamp", same, "Timestamp", "Timestamp"},
		{"a narrow native against the wide type", "Timestamp", "Timestamp64", changed, "Timestamp64", "Timestamp"},
		{"a decimal keeps its arguments", "DECIMAL(10,2)", "Decimal(10,2)", same, "Decimal(10,2)", "Decimal(10,2)"},
		{"another decimal", "DECIMAL(10,2)", "Decimal(12,2)", changed, "Decimal(12,2)", "Decimal(10,2)"},
		{"no counterpart", "TIME", "Utf8", changed, "Utf8", "TIME"},
		{"a vector is a String", "vector(1536)", "String", same, "String", "String"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := requestFor(&protocol.DeclaredColumn{Name: "desired", Type: tc.declared}, &protocol.ObservedColumn{Name: "held", DataType: tc.catalog}, protocol.ColumnAspect_COLUMN_TYPE)
			result, err := columnsemantics.Compare(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Tables, qt.HasLen, 1)
			c.Assert(result.Tables[0].Columns, qt.HasLen, 1)
			c.Assert(result.Tables[0].Columns[0].Aspects, qt.HasLen, 1)
			got, want := result.Tables[0].Columns[0].Aspects[0], diff(protocol.ColumnAspect_COLUMN_TYPE, tc.relation, tc.current, tc.desired)
			c.Assert(proto.Equal(got, want), qt.IsTrue, qt.Commentf("got %v want %v", got, want))
		})
	}
}
