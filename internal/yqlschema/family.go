package yqlschema

import (
	"maps"
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

func (p *parser) family() ydbdecl.ColumnFamily {
	name := decodedName(p.identifier())
	values := map[string]string{"name": name}
	raw := make(map[string]string)
	if p.pos+1 < len(p.tokens) && p.peek().Value == "(" && p.tokens[p.pos+1].Value == ")" {
		p.pos += 2
	} else {
		raw = p.options()
	}
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		switch key {
		case "data", "compression", "cache_mode":
			values[key] = scalar(raw[key])
		default:
			p.failf("unsupported column-family setting %q", key)
		}
	}
	spec, err := ydbdecl.ParseFamily(values)
	if err != nil {
		p.failf("%v", err)
	}
	if spec.Name != name {
		p.failf("column-family name %q cannot be represented exactly", name)
	}
	return spec
}

// bindFamilies gives each declared family the columns that name it and
// attaches the families to table. A family declared twice and a column
// naming a family the statement does not declare are refused, as YDB refuses
// them.
func (p *parser) bindFamilies(table *createTable, families []ydbdecl.ColumnFamily, columns map[string][]string) {
	declared := make(map[string]bool)
	for i := range families {
		family := &families[i]
		if declared[family.Name] {
			p.failf("column family %q is declared twice", family.Name)
		}
		declared[family.Name] = true
		family.Columns = columns[family.Name]
	}
	for _, name := range slices.Sorted(maps.Keys(columns)) {
		if !declared[name] {
			p.failf("column family %q is not declared", name)
		}
	}
	if len(families) == 0 || p.err != nil {
		return
	}
	if err := ydbdecl.ValidateFamilies(families); err != nil {
		p.failf("%v", err)
		return
	}
	table.facets.Families = families
}
