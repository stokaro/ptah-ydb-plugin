package yqlschema

import (
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

func (p *parser) alterSerialSequence() *alterSequence {
	node := &alterSequence{name: decodedName(p.identifier())}
	if !strings.HasPrefix(node.name, "/") || node.name != strings.TrimSpace(node.name) {
		p.failf("a Serial sequence needs its absolute database path")
	}
	for !p.done() && p.peek().Type != yql.TokenSemicolon {
		switch {
		case p.word("START"):
			if node.start != nil {
				p.failf("duplicate sequence START")
			}
			p.pos++
			if p.word("WITH") {
				p.pos++
			}
			value := p.sequenceNumber()
			node.start = &value
		case p.word("INCREMENT"):
			if node.increment != nil {
				p.failf("duplicate sequence INCREMENT")
			}
			p.pos++
			if p.word("BY") {
				p.pos++
			}
			value := p.sequenceNumber()
			node.increment = &value
		default:
			p.failf("desired Serial sequences accept START and INCREMENT; RESTART is an operator action")
		}
	}
	if node.start == nil && node.increment == nil {
		p.failf("ALTER SEQUENCE needs START or INCREMENT")
	}
	return node
}

func (p *parser) sequenceNumber() int64 {
	value, err := strconv.ParseInt(p.peek().Value, 10, 64)
	if err != nil || value < 1 {
		p.failf("expected a positive sequence integer")
	}
	if !p.done() {
		p.pos++
	}
	return value
}
