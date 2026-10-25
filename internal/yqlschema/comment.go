package yqlschema

import (
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// comment uses the same grammar and value limits as the statement executor.
// Conversion resolves the target against this document's objects.
func (p *parser) comment() *commentOn {
	start, end := p.peek().Start, p.peek().Start
	for !p.done() && p.peek().Type != yql.TokenSemicolon {
		end = p.peek().End
		p.pos++
	}
	text := p.text[start:end]
	query, recognized, err := ydbcomment.Recognize(text)
	switch {
	case err != nil:
		p.failf("%v", err)
	case !recognized:
		p.failf("unsupported YDB comment declaration")
	case strings.HasPrefix(query.Path, "/"):
		p.failf("desired YQL schema paths must be database-relative")
	}
	return &commentOn{text: text}
}
