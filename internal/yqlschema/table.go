package yqlschema

import (
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

func (p *parser) table() *createTable {
	guard := p.word("IF")
	if guard {
		p.pos++
		p.wantWord("NOT")
		p.wantWord("EXISTS")
	}
	table := &createTable{name: p.path(), ifNotExists: guard}
	p.want("(")
	var key []string
	var families []ydbdecl.ColumnFamily
	familyColumns := make(map[string][]string)
	for !p.done() && p.peek().Value != ")" {
		switch {
		case p.word("PRIMARY"):
			p.pos++
			p.wantWord("KEY")
			if key != nil {
				p.failf("a table cannot declare its primary key twice")
			}
			key = p.names()
		case p.word("INDEX"):
			p.pos++
			table.indexes = append(table.indexes, p.index(table.name))
		case p.word("FAMILY"):
			p.pos++
			families = append(families, p.family())
		case p.anyWord([]string{"CONSTRAINT", "UNIQUE", "CHECK", "FOREIGN"}):
			p.failf("this table element is not supported in a desired YQL schema")
		default:
			table.columns = append(table.columns, p.column(familyColumns))
		}
		if !p.accept(",") {
			break
		}
	}
	p.want(")")
	p.primaryKey(table, key)
	p.bindFamilies(table, families, familyColumns)
	p.tableSettings(table)
	return table
}

func (p *parser) column(families map[string][]string) *column {
	column := &column{name: p.identifier(), typ: p.identifier(), nullable: true}
	if p.peek().Value == "(" {
		column.typ += p.typeParameters()
	}
	seen := make(map[string]bool)
	for !p.done() && p.peek().Value != "," && p.peek().Value != ")" {
		clause := strings.ToUpper(p.peek().Value)
		if seen[clause] {
			p.failf("column clause %s is declared twice", clause)
			break
		}
		seen[clause] = true
		switch {
		case p.word("NOT"):
			p.pos++
			p.wantWord("NULL")
			column.nullable = false
		case p.word("FAMILY"):
			p.pos++
			family := decodedName(p.identifier())
			if family != "default" {
				families[family] = append(families[family], decodedName(column.name))
			}
		case p.word("DEFAULT"):
			p.pos++
			literal := p.expression("NOT", "FAMILY")
			value, err := defaultValue(literal, column.typ)
			if err != nil {
				p.failf("%v", err)
				break
			}
			column.defaultValue = &value
		default:
			p.failf("unsupported YQL column clause")
		}
	}
	return column
}

func (p *parser) typeParameters() string {
	start := p.peek().Start
	p.want("(")
	for !p.done() && p.peek().Value != ")" {
		token := p.peek()
		if token.Type != yql.TokenIdentifier && token.Value != "," {
			p.failf("unsupported type parameter")
			break
		}
		p.pos++
	}
	end := p.peek().End
	p.want(")")
	return strings.ReplaceAll(p.text[start:end], " ", "")
}

func (p *parser) primaryKey(table *createTable, names []string) {
	if len(names) == 0 {
		p.failf("a YDB table needs a primary key")
		return
	}
	columns := make(map[string]*column)
	for _, column := range table.columns {
		name := decodedName(column.name)
		if columns[name] != nil {
			p.failf("column %q is declared twice", name)
		}
		columns[name] = column
	}
	seen := make(map[string]bool)
	for _, raw := range names {
		name := decodedName(raw)
		column := columns[name]
		switch {
		case seen[name]:
			p.failf("primary key column %q is declared twice", name)
		case column == nil:
			p.failf("primary key names unknown column %q", name)
		case column.nullable:
			p.failf("nullable primary key column %q cannot be represented by a desired schema; declare NOT NULL", name)
		}
		seen[name] = true
	}
	table.primaryKey = names
}
