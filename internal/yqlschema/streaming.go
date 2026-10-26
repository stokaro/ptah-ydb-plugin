package yqlschema

import (
	"fmt"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

func (c *converter) declareStreaming(node *createStreamingQuery) error {
	if err := streaming.ValidateIdentity(node.schema, node.name); err != nil {
		return fmt.Errorf("invalid feature value: streaming query requires a schema-scoped YDB identity")
	}
	object := Object{Kind: KindStreamingQuery, Schema: node.schema, Name: node.name, Spec: node, span: c.statement, document: c.document}
	for _, reading := range c.readings() {
		if held := reading.object(object); held != nil {
			if node.ifNotExists {
				return nil
			}
			if node.replace {
				*held = object
				return nil
			}
			return fmt.Errorf("streaming query %q is declared twice", canonicalReference(node.schema, node.name))
		}
	}
	return c.declare(object)
}

type createStreamingQuery struct {
	spanned
	schema, name         string
	spec                 streaming.Spec
	allowStateReset      bool
	replace, ifNotExists bool
}

func (p *parser) streamingQuery(replace bool) *createStreamingQuery {
	p.wantWord("STREAMING")
	p.wantWord("QUERY")
	guarded := p.word("IF")
	if guarded {
		p.pos++
		p.wantWord("NOT")
		p.wantWord("EXISTS")
	}
	name, directory := decodedName(p.path()), ""
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		directory, name = name[:slash], name[slash+1:]
	}
	node := &createStreamingQuery{schema: directory, name: name, allowStateReset: replace && !guarded, replace: replace, ifNotExists: guarded}
	if p.word("WITH") {
		p.pos++
		p.optionsUsing(func(key string) string {
			value := p.expression()
			switch key {
			case "run":
				switch {
				case strings.EqualFold(value, "TRUE"):
					node.spec.Run = new(true)
				case strings.EqualFold(value, "FALSE"):
					node.spec.Run = new(false)
				default:
					p.failf("streaming query run requires TRUE or FALSE")
				}
			case "resource_pool":
				pool, ok := stringLiteral(value, "String")
				literal := newParser(value)
				if len(literal.tokens) == 1 && literal.tokens[0].Type == yql.TokenIdentifier {
					pool, ok = yql.IdentifierValue(value)
				}
				if !ok || pool == "" || strings.HasPrefix(pool, "$") {
					p.failf("streaming query resource_pool requires a literal name")
				}
				node.spec.ResourcePool = pool
			default:
				p.failf("unsupported streaming query setting %q", key)
			}
			return value
		})
	}
	p.wantWord("AS")
	block := newParser(p.queryBody())
	tokens := block.tokens
	if block.err != nil || len(tokens) < 4 || !tokens[0].IsWord("DO") || !tokens[1].IsWord("BEGIN") || !tokens[len(tokens)-2].IsWord("END") || !tokens[len(tokens)-1].IsWord("DO") {
		p.failf("a streaming query requires AS DO BEGIN ... END DO")
		return node
	}
	node.spec.Text = strings.TrimSpace(block.text[tokens[1].End:tokens[len(tokens)-2].Start])
	if err := streaming.Validate(node.spec); err != nil {
		p.failf("%v", err)
	}
	return node
}
