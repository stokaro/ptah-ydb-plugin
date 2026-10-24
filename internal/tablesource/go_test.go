package tablesource_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// fileSize is the length of the imaginary Go file the declarations sit in.
const fileSize = 10000

// declaration is a columnfamily directive on its own line of the file, the
// line number being its position in the list.
func declaration(line int, structName string, attributes map[string]string) testsource.GoDeclaration {
	start := (line - 1) * 100
	return testsource.GoDeclaration{Directive: "ptah:schema:columnfamily", Attributes: attributes, Struct: structName, File: "entities.go", FileSize: fileSize,
		Start: testsource.GoPosition{Offset: start, Line: line, Column: 1}, End: testsource.GoPosition{Offset: start + 40, Line: line, Column: 41}}
}

var fileTables = []testsource.GoTable{{Name: "items", Struct: "Item"}, {Schema: "shop", Name: "orders", Struct: "Order"}, {Schema: "archive", Name: "orders", Struct: "OldOrder"}}

func goRequest(c *qt.C, declarations ...testsource.GoDeclaration) *protocol.DecodeSourceRequest {
	c.Helper()
	request, err := testsource.GoDirectives(fileTables, declarations)
	c.Assert(err, qt.IsNil)
	return request
}

func TestGoDirectives_HappyPath(t *testing.T) {
	tests := []struct {
		name         string
		declarations []testsource.GoDeclaration
		wantOwner    string
		wantPayload  string
	}{
		{"on the struct", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "data": "hdd", "compression": "LZ4"})},
			".items", `{"columnFamilies":[{"compression":"COMPRESSION_LZ4","data":{"media":"hdd"},"name":"cold"}]}`},
		{"several on one table keep their order", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "default", "compression": "off"}),
			declaration(3, "Item", map[string]string{"name": "cold"})}, ".items", `{"columnFamilies":[{"compression":"COMPRESSION_NONE","name":"default"},{"name":"cold"}]}`},
		{"on another table of the file, qualified", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "table": "shop.orders"})},
			"shop.orders", `{"columnFamilies":[{"name":"cold"}]}`},
		{"on another table of the file, by a name only one schema has", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "table": "items"})},
			".items", `{"columnFamilies":[{"name":"cold"}]}`},
		{"quoted parts", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "table": `"archive"."orders"`})},
			"archive.orders", `{"columnFamilies":[{"name":"cold"}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := goRequest(c, test.declarations...)
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
			var owners, payloads []string
			for _, fragment := range decoded.Documents[0].Fragments {
				for _, value := range fragment.Values {
					owners = append(owners, value.Value.Value.Owner.Schema+"."+value.Value.Value.Owner.Name)
					payloads = append(payloads, string(value.Value.Value.Payload))
				}
			}
			c.Assert(owners, qt.DeepEquals, []string{test.wantOwner})
			c.Assert(payloads, qt.DeepEquals, []string{test.wantPayload})
		})
	}
}

// A family that lists columns places each of them, and the host attaches the
// column facet to the field of the struct the directive is written on.
func TestGoDirectives_FamilyPlacesItsColumns(t *testing.T) {
	c := qt.New(t)
	request := goRequest(c,
		declaration(2, "Item", map[string]string{"name": "cold", "data": "hdd", "fields": "body, notes"}),
		declaration(3, "Item", map[string]string{"name": "warm", "fields": "title"}),
		declaration(4, "Order", map[string]string{"name": "cold", "fields": "body"}))

	decoded, err := backend.New().DecodeSource(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
	var placed []string
	for _, fragment := range decoded.Documents[0].Fragments {
		for _, value := range fragment.Values {
			owner := value.Value.Value.Owner
			placed = append(placed, fragment.Id+" "+value.Value.Value.Kind+" "+owner.Kind+" "+owner.Schema+"."+strings.Join(owner.Path, "/")+"."+owner.Name+" "+string(value.Value.Value.Payload))
		}
	}
	c.Assert(placed, qt.DeepEquals, []string{
		`0 github.com/stokaro/ptah-ydb-plugin/row-table-properties table ..items {"columnFamilies":[{"data":{"media":"hdd"},"name":"cold"},{"name":"warm"}]}`,
		`0 github.com/stokaro/ptah-ydb-plugin/column-properties column .items.body {"family":"cold"}`,
		`0 github.com/stokaro/ptah-ydb-plugin/column-properties column .items.notes {"family":"cold"}`,
		`0 github.com/stokaro/ptah-ydb-plugin/column-properties column .items.title {"family":"warm"}`,
		`2 github.com/stokaro/ptah-ydb-plugin/row-table-properties table shop..orders {"columnFamilies":[{"name":"cold"}]}`,
		`2 github.com/stokaro/ptah-ydb-plugin/column-properties column shop.orders.body {"family":"cold"}`,
	})
}

func TestGoDirectives_TablesGetTheirOwnFacets(t *testing.T) {
	c := qt.New(t)
	request := goRequest(c,
		declaration(2, "Item", map[string]string{"name": "cold"}),
		declaration(3, "Order", map[string]string{"name": "hot", "data": "ssd"}),
		declaration(4, "Item", map[string]string{"name": "warm", "table": "shop.orders"}))
	tables, err := tablesource.Decode(c.Context(), request.Documents[0])
	c.Assert(err, qt.IsNil)
	c.Assert(len(tables), qt.Equals, 2)
	c.Assert(tables[0].Fragment, qt.Equals, 0)
	c.Assert(tables[0].Binding.Object.Name, qt.Equals, "items")
	c.Assert(string(tables[0].Value.CanonicalProjection), qt.Equals, `{"columnFamilies":[{"name":"cold"}]}`)
	c.Assert(tables[1].Fragment, qt.Equals, 1)
	c.Assert(string(tables[1].Value.CanonicalProjection), qt.Equals, `{"columnFamilies":[{"data":{"media":"ssd"},"name":"hot"},{"name":"warm"}]}`)
	ids := map[string]bool{}
	for _, table := range tables {
		ids[table.Value.Id] = true
	}
	c.Assert(ids, qt.HasLen, 2)
}

func TestGoDirectives_FailurePath(t *testing.T) {
	tests := []struct {
		name         string
		declarations []testsource.GoDeclaration
		wantErr      string
	}{
		{"a name only", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"table": "items"})},
			`table "items": column family "": invalid name "": a column family needs a name`},
		{"the same family twice", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold"}), declaration(3, "Item", map[string]string{"name": "cold", "data": "hdd"})},
			`table "items": column family "cold": it declares column family "cold" twice`},
		{"the same family twice through the table attribute", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "table": "shop.orders"}),
			declaration(3, "Order", map[string]string{"name": "cold"})}, `table "shop.orders": column family "cold": it declares column family "cold" twice`},
		{"an empty storage pool", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "data": ""})},
			`table "items": column family "cold": invalid data "": names the kind of storage pool.*`},
		{"a compression a row table lacks", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "compression": "zstd"})},
			`table "items": column family "cold": invalid compression "zstd": takes off or lz4: YDB keeps zstd.*`},
		{"an unknown compression", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "compression": "gzip"})},
			`table "items": column family "cold": invalid compression "gzip": takes off or lz4`},
		{"a table the file does not declare", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "table": "missing"})},
			`fragment "0": table "missing" is not declared in this file, and a column family is declared beside its table`},
		{"a name two schemas share", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "table": "orders"})},
			`fragment "0": table "orders" is declared in schemas "shop" and "archive"; name the schema in the table attribute`},
		{"a table with too many parts", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "table": "a.b.c"})},
			`fragment "0": invalid table "a.b.c": write the table as name or schema.name, quoting a part that holds a dot`},
		{"a quote left open", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "table": `"shop.orders`})},
			`fragment "0": invalid table "\\"shop.orders": a quoted name is not closed`},
		{"a cache mode", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "cache_mode": "regular"})},
			`table "items": column family "cold": cache_mode is not supported by the YDB provider yet: .*`},
		{"an empty column name", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "fields": "a,,b"})},
			`table "items": column family "cold": invalid fields "a,,b": takes a comma-separated list of column names, none of them empty`},
		{"an empty list", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "fields": ""})},
			`table "items": column family "cold": invalid fields "": takes a comma-separated list of column names, none of them empty`},
		{"a column named twice", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "fields": "a, b ,a"})},
			`table "items": column family "cold": invalid fields "a, b ,a": names column "a" twice`},
		{"a column in two families", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "fields": "a,b"}),
			declaration(3, "Item", map[string]string{"name": "warm", "fields": "b"})},
			`table "items": column family "warm": column "b" is in two column families, "cold" and "warm"`},
		{"columns in the default family", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "default", "fields": "a"})},
			`table "items": column family "default": invalid fields "a": the default family holds the key and every column no other family names, so it lists none`},
		{"columns of another table", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "table": "shop.orders", "fields": "a"})},
			`table "shop.orders": fields is not supported by the YDB provider yet: a family places the columns of the table of the struct its directive is written on.*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := goRequest(c, test.declarations...)
			tables, err := tablesource.Decode(c.Context(), request.Documents[0])
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(len(tables), qt.Equals, 0)
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(decoded == nil, qt.IsTrue)
		})
	}
}

// The host refuses an attribute outside the claim before it dispatches, and
// the SDK validator refuses it again on the way in.
func TestGoDirectives_AttributeOutsideTheClaim(t *testing.T) {
	c := qt.New(t)
	request := goRequest(c, declaration(2, "Item", map[string]string{"name": "cold", "barriers": "x"}))
	decoded, err := backend.New().DecodeSource(c.Context(), request)
	c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
	c.Assert(decoded == nil, qt.IsTrue)
}

func TestGoDirectives_ChangefeedsAreRefusedByName(t *testing.T) {
	for _, directive := range []string{"ptah:schema:changefeed", "ptah:schema:changefeed:consumer"} {
		t.Run(directive, func(t *testing.T) {
			c := qt.New(t)
			written := declaration(2, "Item", map[string]string{"name": "updates"})
			written.Directive = directive
			request := goRequest(c, written)
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.ErrorMatches, `(?s).*`+directive+` is not supported by the YDB provider yet: the provider plans no changefeed yet`)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(decoded == nil, qt.IsTrue)
		})
	}
}

// attributes is a ptah:schema:table directive on its own line of the file,
// carrying only the attributes the provider claimed, as the host sends them.
func tableDeclaration(line int, structName string, attributes map[string]string) testsource.GoDeclaration {
	written := declaration(line, structName, attributes)
	written.Directive = "ptah:schema:table"
	return written
}

func indexDeclaration(line int, structName string, attributes map[string]string) testsource.GoDeclaration {
	written := declaration(line, structName, attributes)
	written.Directive = "ptah:schema:index"
	written.Subject = testsource.GoSubject{Kind: "index", Name: "by_body"}
	return written
}

func TestTableDirective_HappyPath(t *testing.T) {
	tests := []struct {
		name         string
		declarations []testsource.GoDeclaration
		wantPayload  string
	}{
		{"partitioning", []testsource.GoDeclaration{tableDeclaration(2, "Item", map[string]string{"auto_partitioning_by_load": "enabled", "auto_partitioning_min_partitions_count": "2"})},
			`{"partitioningSettings":{"minPartitionsCount":"2","partitioningByLoad":"ENABLED"}}`},
		{"replicas and bloom filter", []testsource.GoDeclaration{tableDeclaration(2, "Item", map[string]string{"read_replicas_settings": "per_az:2", "key_bloom_filter": "DISABLED"})},
			`{"keyBloomFilter":"DISABLED","readReplicasSettings":{"perAzReadReplicasCount":"2"}}`},
		{"a TTL on a date column", []testsource.GoDeclaration{tableDeclaration(2, "Item", map[string]string{
			"platform.ydb.row_deletion_column": " created_at ", "platform.ydb.row_deletion_interval": "P30D"})},
			`{"ttlSettings":{"dateTypeColumn":{"columnName":"created_at","expireAfterSeconds":2592000}}}`},
		{"a TTL on an integer column", []testsource.GoDeclaration{tableDeclaration(2, "Item", map[string]string{
			"platform.ydb.row_deletion_column": "expires", "platform.ydb.row_deletion_interval": "PT1H30M", "platform.ydb.row_deletion_unit": "milliseconds"})},
			`{"ttlSettings":{"valueSinceUnixEpoch":{"columnName":"expires","columnUnit":"UNIT_MILLISECONDS","expireAfterSeconds":5400}}}`},
		{"row storage names nothing", []testsource.GoDeclaration{tableDeclaration(2, "Item", map[string]string{"store": " row ", "key_bloom_filter": "ENABLED"})},
			`{"keyBloomFilter":"ENABLED"}`},
		{"attributes and a family join one facet", []testsource.GoDeclaration{
			declaration(2, "Item", map[string]string{"name": "cold", "data": "hdd"}),
			tableDeclaration(3, "Item", map[string]string{"auto_partitioning_by_size": "DISABLED", "platform.ydb.row_deletion_column": "created_at", "platform.ydb.row_deletion_interval": "PT0S"})},
			`{"columnFamilies":[{"data":{"media":"hdd"},"name":"cold"}],"partitioningSettings":{"partitioningBySize":"DISABLED"},` +
				`"ttlSettings":{"dateTypeColumn":{"columnName":"created_at"}}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := goRequest(c, test.declarations...)
			tables, err := tablesource.Decode(c.Context(), request.Documents[0])
			c.Assert(err, qt.IsNil)
			c.Assert(tables, qt.HasLen, 1)
			c.Assert(string(tables[0].Value.CanonicalProjection), qt.Equals, test.wantPayload)
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
		})
	}
}

// A table that states row storage and nothing else declares nothing, so the
// host gets no facet for it and the table's settings stay as they are.
func TestTableDirective_RowStorageAloneDeclaresNothing(t *testing.T) {
	c := qt.New(t)
	request := goRequest(c, tableDeclaration(2, "Item", map[string]string{"store": "ROW"}))
	tables, err := tablesource.Decode(c.Context(), request.Documents[0])
	c.Assert(err, qt.IsNil)
	c.Assert(tables, qt.HasLen, 0)
}

func TestTableDirective_FailurePath(t *testing.T) {
	ttl := func(column, interval, unit string) map[string]string {
		attributes := map[string]string{}
		for name, value := range map[string]string{"platform.ydb.row_deletion_column": column, "platform.ydb.row_deletion_interval": interval, "platform.ydb.row_deletion_unit": unit} {
			if value != "-" {
				attributes[name] = value
			}
		}
		return attributes
	}
	tests := []struct {
		name       string
		attributes map[string]string
		wantErr    string
	}{
		{"a switch that is no switch", map[string]string{"auto_partitioning_by_size": "yes"}, `table "items": invalid auto_partitioning_by_size "yes": write ENABLED or DISABLED`},
		{"a count of zero", map[string]string{"auto_partitioning_min_partitions_count": "0"}, `table "items": invalid auto_partitioning_min_partitions_count "0": write a whole number of at least 1.*`},
		{"replicas without a mode", map[string]string{"read_replicas_settings": "2"}, `table "items": invalid read_replicas_settings "2": write PER_AZ:<n>.*`},
		{"a starting layout by count", map[string]string{"uniform_partitions": "4"}, `table "items": uniform_partitions is not supported by the YDB provider yet: .*`},
		{"a starting layout by key", map[string]string{"partition_at_keys": "10, 20"}, `table "items": partition_at_keys is not supported by the YDB provider yet: .*`},
		{"a column table", map[string]string{"store": "column"}, `table "items": store is not supported by the YDB provider yet: .*`},
		{"a column table with a hash key", map[string]string{"store": "COLUMN", "partition_by_hash": "id", "column_shards": "4"}, `table "items": store is not supported by the YDB provider yet: .*`},
		{"a storage kind that does not exist", map[string]string{"store": "colum"}, `table "items": invalid store "colum": write ROW or COLUMN`},
		{"an empty storage kind", map[string]string{"store": " "}, `table "items": invalid store " ": write ROW or COLUMN`},
		{"a hash key on a row table", map[string]string{"partition_by_hash": "id"}, `table "items": invalid partition_by_hash "id": it describes a column table, so write store=COLUMN`},
		{"shards on a row table", map[string]string{"store": "ROW", "column_shards": "4"}, `table "items": invalid column_shards "4": it describes a column table.*`},
		{"a retention policy on a row table", map[string]string{"column_ttl": "{}"}, `table "items": invalid column_ttl "\{\}": it describes a column table.*`},
		{"a TTL without an interval", ttl("created_at", "-", "-"), `table "items": invalid platform.ydb.row_deletion_interval "": a TTL needs the interval.*`},
		{"a TTL without a column", ttl("-", "P30D", "-"), `table "items": invalid platform.ydb.row_deletion_column "": a TTL needs the column.*`},
		{"a unit alone", ttl("-", "-", "SECONDS"), `table "items": invalid platform.ydb.row_deletion_column "": a TTL needs the column.*`},
		{"an empty column", ttl("", "P30D", "-"), `table "items": invalid platform.ydb.row_deletion_column "": it is empty.*`},
		{"an empty unit", ttl("created_at", "PT1H", ""), `table "items": invalid platform.ydb.row_deletion_unit "": it is empty.*`},
		{"an interval in months", ttl("created_at", "P1M", "-"), `table "items": invalid platform.ydb.row_deletion_interval "P1M": .*designator "M" is out of place.*`},
		{"an interval with a fraction", ttl("created_at", "PT1.5S", "-"), `table "items": invalid platform.ydb.row_deletion_interval "PT1.5S": .*fraction of a second.*`},
		{"an unknown unit", ttl("created_at", "PT1H", "ms"), `table "items": invalid platform.ydb.row_deletion_unit "ms": unit "ms" is not one YDB takes.*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := goRequest(c, tableDeclaration(2, "Item", test.attributes))
			tables, err := tablesource.Decode(c.Context(), request.Documents[0])
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(tables, qt.HasLen, 0)
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(decoded == nil, qt.IsTrue)
		})
	}
}

// The provider claims the attributes of an index directive in order to refuse
// them by name: the row-table model holds no index option, so one accepted
// would be dropped.
func TestIndexDirective_RefusedByName(t *testing.T) {
	for _, attribute := range []string{"auto_partitioning_by_load", "read_replicas_settings", "tokenizer", "false_positive_probability", "distance", "clusters"} {
		t.Run(attribute, func(t *testing.T) {
			c := qt.New(t)
			request := goRequest(c, indexDeclaration(2, "Item", map[string]string{attribute: "x"}))
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.ErrorMatches, `(?s).*`+attribute+` is not supported by the YDB provider yet: an index states no settings of its own.*`)
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(decoded == nil, qt.IsTrue)
		})
	}
}
