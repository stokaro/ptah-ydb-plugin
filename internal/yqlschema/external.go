package yqlschema

import (
	"maps"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// external reads `CREATE [OR REPLACE] EXTERNAL DATA SOURCE` or `EXTERNAL
// TABLE`. A dot is part of a name and a path written absolute is refused. OR
// REPLACE says nothing a declaration keeps.
func (p *parser) external() []statement {
	if p.word("TABLE") {
		p.pos++
		return one(p.externalTable())
	}
	p.wantWord("DATA")
	p.wantWord("SOURCE")
	ref := p.externalPath()
	values := p.externalSettings()
	source := ydbdecl.ExternalDataSource{
		SourceType: p.requiredExternalSetting(values, "SOURCE_TYPE"),
		Location:   takeExternalSetting(values, "LOCATION"),
		AuthMethod: p.requiredExternalSetting(values, "AUTH_METHOD"),
	}
	if len(values) > 0 {
		source.Options = values
	}
	return one(&createExternalSource{schema: ref.Schema, name: ref.Name, spec: source})
}

func (p *parser) externalTable() *createExternalTable {
	ref := p.externalPath()
	p.want("(")
	var table ydbdecl.ExternalTable
	for !p.done() && p.peek().Value != ")" {
		if p.anyWord([]string{"PRIMARY", "FOREIGN"}) && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].IsWord("KEY") {
			p.failf("external tables cannot declare keys")
			break
		}
		column := ydbdecl.ExternalColumn{Name: decodedName(p.identifier()), Type: p.identifier()}
		if p.peek().Value == "(" {
			column.Type += p.typeParameters()
		}
		if p.word("NOT") {
			p.pos++
			p.wantWord("NULL")
			column.NotNull = true
		}
		table.Columns = append(table.Columns, column)
		if !p.accept(",") {
			break
		}
	}
	p.want(")")
	if err := ydbdecl.CheckExternalColumns(table.Columns); err != nil {
		p.failf("%v", err)
	}
	values := p.externalSettings()
	table.DataSource = p.requiredExternalSetting(values, "DATA_SOURCE")
	table.Location = p.requiredExternalSetting(values, "LOCATION")
	if len(values) > 0 {
		table.Options = values
	}
	return &createExternalTable{schema: ref.Schema, name: ref.Name, spec: table}
}

// externalPath reads the path of an external object relative to the database
// root.
func (p *parser) externalPath() ydbdecl.Path {
	ref, err := ydbdecl.ParseExternalPath(decodedName(p.path()))
	if err != nil {
		p.failf("%v", err)
	}
	return ref
}

func (p *parser) externalSettings() map[string]string {
	p.wantWord("WITH")
	raw := p.options()
	values := make(map[string]string, len(raw))
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		value, ok := stringLiteral(raw[key], "String")
		if !ok || strings.HasSuffix(strings.ToLower(raw[key]), "s") {
			p.failf("external object setting %q requires a string literal without a type suffix", key)
		}
		values[strings.ToUpper(key)] = value
	}
	// The quoted YQL values already have their boundaries, so their contents
	// are kept without trimming.
	if _, err := ydbdecl.CheckExternalOptions(values); err != nil {
		p.failf("%v", err)
	}
	return values
}

func (p *parser) requiredExternalSetting(values map[string]string, name string) string {
	value := takeExternalSetting(values, name)
	if strings.TrimSpace(value) == "" {
		p.failf("external object setting %s is required", name)
	}
	return value
}

func takeExternalSetting(values map[string]string, name string) string {
	value := values[name]
	delete(values, name)
	return value
}
