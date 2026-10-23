package tablesource_test

import (
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
		{"column placement", []testsource.GoDeclaration{declaration(2, "Item", map[string]string{"name": "cold", "fields": "body"})},
			`table "items": column family "cold": fields is not supported by the YDB provider yet: .*`},
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
