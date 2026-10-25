package yqlschema

import "strings"

// tableRef is an unqualified or schema-qualified reference, as the host's
// internal/tableref reads one.
type tableRef struct {
	schema, name string
	qualified    bool
}

// parseTableRef reads an unqualified or schema-qualified SQL identifier
// reference. It accepts SQL-standard quotes, backticks and brackets.
func parseTableRef(value string) (tableRef, bool) {
	parts, ok := splitReference(strings.TrimSpace(value))
	if !ok || len(parts) == 0 || len(parts) > 2 {
		return tableRef{}, false
	}
	if len(parts) == 1 {
		return tableRef{name: parts[0]}, parts[0] != ""
	}
	return tableRef{schema: parts[0], name: parts[1], qualified: true}, parts[0] != "" && parts[1] != ""
}

// canonicalReference formats schema and name without losing literal
// identifier dots.
func canonicalReference(schema, name string) string {
	schema = strings.TrimSpace(schema)
	name = strings.TrimSpace(name)
	if schema == "" {
		return canonicalPart(name)
	}
	return canonicalPart(schema) + "." + canonicalPart(name)
}

func canonicalPart(value string) string {
	if !strings.ContainsAny(value, ".\"`[]") {
		return value
	}
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func splitReference(value string) ([]string, bool) {
	parser := referenceParser{parts: make([]string, 0, 2)}
	for position := 0; position < len(value); position++ {
		if !parser.consume(value, &position) {
			return nil, false
		}
	}
	return parser.result()
}

type referenceParser struct {
	parts            []string
	part             strings.Builder
	quote            byte
	partQuoted       bool
	quotedPartClosed bool
}

func (p *referenceParser) consume(value string, position *int) bool {
	if p.quote != 0 {
		return p.consumeQuoted(value, position)
	}
	return p.consumeUnquoted(value[*position])
}

func (p *referenceParser) consumeUnquoted(character byte) bool {
	if p.quotedPartClosed && character != '.' {
		return isReferenceSpace(character)
	}
	switch character {
	case '"', '`', '[':
		if strings.TrimSpace(p.part.String()) != "" {
			p.part.WriteByte(character)
			return true
		}
		p.part.Reset()
		p.quote = character
		p.partQuoted = true
	case '.':
		if !p.appendPart() {
			return false
		}
		p.part.Reset()
		p.partQuoted = false
		p.quotedPartClosed = false
	default:
		p.part.WriteByte(character)
	}
	return true
}

func (p *referenceParser) consumeQuoted(value string, position *int) bool {
	character := value[*position]
	closeQuote := p.quote
	if p.quote == '[' {
		closeQuote = ']'
	}
	if character != closeQuote {
		p.part.WriteByte(character)
		return true
	}
	if *position+1 < len(value) && value[*position+1] == closeQuote {
		p.part.WriteByte(closeQuote)
		(*position)++
		return true
	}
	p.quote = 0
	p.quotedPartClosed = true
	return true
}

func (p *referenceParser) result() ([]string, bool) {
	if p.quote != 0 || !p.appendPart() {
		return nil, false
	}
	return p.parts, true
}

func (p *referenceParser) appendPart() bool {
	value := p.part.String()
	if !p.partQuoted {
		value = strings.TrimSpace(value)
	}
	if value == "" {
		return false
	}
	p.parts = append(p.parts, value)
	return true
}

func isReferenceSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\r' || character == '\n'
}
