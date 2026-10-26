package tablesource_test

import (
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablesource"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

func TestYAMLFamilyFields(t *testing.T) {
	for _, test := range []struct {
		name, fields string
		columns      []string
	}{
		{"sequence", `["body", " notes "]`, []string{"body", "notes"}},
		{"scalar", `"body, notes"`, []string{"body", "notes"}},
		{"scalar types", `[1, true]`, []string{"1", "true"}},
		{"null", `null`, nil},
		{"empty scalar", `""`, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			request := yamlRequest(c, fmt.Sprintf(`{"tables":{"items":{"key_bloom_filter":"ENABLED","column_families":{"cold":{"fields":%s}}}}}`, test.fields))
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
			var columns []string
			for _, fragment := range decoded.Documents[0].Fragments {
				for _, value := range fragment.Values {
					if value.Value.Value.Kind != schema.ColumnProperties {
						continue
					}
					columns = append(columns, value.Value.NormalizedOwner.Name)
					c.Assert(fragment.Id, qt.Equals, "items/column_families")
					c.Assert(value.Source.FragmentId, qt.Equals, fragment.Id)
					c.Assert(value.Value.NormalizedOwner.Path, qt.DeepEquals, []string{"items"})
					c.Assert(string(value.Value.CanonicalProjection), qt.Equals, `{"family":"cold"}`)
				}
			}
			c.Assert(columns, qt.DeepEquals, test.columns)
		})
	}
}

func TestFamilyColumnsFollowTheirClaim(t *testing.T) {
	for _, tableFirst := range []bool{true, false} {
		t.Run(fmt.Sprint(tableFirst), func(t *testing.T) {
			c := qt.New(t)
			table := declaration(1, "Item", map[string]string{"key_bloom_filter": "ENABLED"})
			table.Directive = tablesource.DirectiveTable
			family := declaration(2, "Item", map[string]string{"name": "cold", "fields": "body"})
			request := goRequest(c, table, family)
			columnFragment := 1
			if !tableFirst {
				request = goRequest(c, family, table)
				columnFragment = 0
			}
			decoded, err := backend.New().DecodeSource(c.Context(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateDecodedSource(backend.Description(), request, decoded), qt.IsNil)
			var columns []*protocol.SourceModelValue
			for _, value := range decoded.Documents[0].Fragments[columnFragment].Values {
				if value.Value.Value.Kind == schema.ColumnProperties {
					columns = append(columns, value)
				}
			}
			c.Assert(columns, qt.HasLen, 1)
			c.Assert(columns[0].Value.NormalizedOwner.Name, qt.Equals, "body")
		})
	}
}
