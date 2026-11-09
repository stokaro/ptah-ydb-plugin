package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// Defaults and typed column defaults are already represented by common
// columns. Family assignment, compression, and encoding are native settings
// that a JSON or DBML export leaves out. Both representations count alike.
func TestCountValue_ColumnOmissions_HappyPath(t *testing.T) {
	for _, row := range []struct {
		name   string
		change func(*native.ColumnMeta)
		want   string
	}{
		{name: "plain", change: func(*native.ColumnMeta) {}, want: "ydb_column_property_omissions=0"},
		{name: "default family", change: func(column *native.ColumnMeta) { column.Family = "default" }, want: "ydb_column_property_omissions=0"},
		{name: "explicitly empty default", change: func(column *native.ColumnMeta) { column.DefaultValue = &native.ColumnMeta_EmptyDefault{} }, want: "ydb_column_property_omissions=0"},
		{name: "literal default", change: func(column *native.ColumnMeta) {
			column.DefaultValue = &native.ColumnMeta_FromLiteral{FromLiteral: &Ydb.TypedValue{Type: primitive(Ydb.Type_INT64), Value: &Ydb.Value{Value: &Ydb.Value_Int64Value{Int64Value: 7}}}}
		}, want: "ydb_column_property_omissions=0"},
		{name: "disabled compression", change: func(column *native.ColumnMeta) {
			column.Compression = &native.ColumnCompression{Algorithm: native.ColumnCompression_ALGORITHM_OFF.Enum()}
		}, want: "ydb_column_property_omissions=0"},
		{name: "disabled encoding", change: func(column *native.ColumnMeta) {
			column.Encoding = []*native.ColumnEncoding{{EncodingSettings: &native.ColumnEncoding_Off_{Off: &native.ColumnEncoding_Off{}}}}
		}, want: "ydb_column_property_omissions=0"},
		{name: "nondefault family", change: func(column *native.ColumnMeta) { column.Family = "cold" }, want: "ydb_column_property_omissions=1"},
		{name: "compression", change: func(column *native.ColumnMeta) {
			column.Compression = &native.ColumnCompression{Algorithm: native.ColumnCompression_ALGORITHM_ZSTD.Enum()}
		}, want: "ydb_column_property_omissions=1"},
		{name: "dictionary encoding", change: func(column *native.ColumnMeta) {
			column.Encoding = []*native.ColumnEncoding{{EncodingSettings: &native.ColumnEncoding_Dictionary_{Dictionary: &native.ColumnEncoding_Dictionary{}}}}
		}, want: "ydb_column_property_omissions=1"},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			column := &native.ColumnMeta{Name: "SerialID", Type: primitive(Ydb.Type_INT64)}
			row.change(column)
			observed, err := schema.CaptureColumn(columnOwner(), column)
			c.Assert(err, qt.IsNil)
			desired, err := schema.ConvertColumn(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
			c.Assert(err, qt.IsNil)
			for _, value := range []struct {
				representation protocol.ValueInput_Representation
				value          *protocol.Envelope
			}{{protocol.ValueInput_OBSERVED, observed}, {protocol.ValueInput_DESIRED, desired}} {
				counts, err := schema.CountValue(value.value, value.representation)
				c.Assert(err, qt.IsNil)
				c.Assert(countText(counts), qt.DeepEquals, []string{row.want})
			}
		})
	}
}
