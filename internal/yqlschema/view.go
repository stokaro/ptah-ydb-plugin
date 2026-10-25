package yqlschema

import (
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

func (p *parser) path() string {
	name := p.identifier()
	if strings.HasPrefix(decodedName(name), "/") {
		p.failf("desired YQL schema paths must be database-relative")
	}
	return name
}

func (p *parser) view() *createView {
	node := &createView{name: p.path()}
	p.wantWord("WITH")
	p.want("(")
	// Unlike ordinary option names, YDB requires this exact case.
	if decodedName(p.identifier()) != "security_invoker" {
		p.failf("a YDB view requires security_invoker = TRUE")
	}
	p.want("=")
	p.wantWord("TRUE")
	p.want(")")
	p.wantWord("AS")
	if !p.word("SELECT") && !p.word("DO") {
		p.failf("a YDB view needs a SELECT or DO query")
	}
	node.body = p.queryBody()
	return node
}

// queryBody retains the query text. Semicolons inside lambdas and inline
// actions belong to the view, as they do to the YQL statement splitter.
func (p *parser) queryBody() string {
	start, end := p.peek().Start, p.peek().Start
	var bodies yql.Bodies
	var closers []string
	for !p.done() {
		token := p.peek()
		if token.Type == yql.TokenSemicolon && !keepSemicolon(&bodies) {
			break
		}
		if token.Type == yql.TokenUnknown {
			p.failf("unrecognized token in query body")
			break
		}
		switch {
		case token.Type == yql.TokenIdentifier:
			bodies.Word(strings.ToUpper(token.Value))
		case token.Type != yql.TokenSemicolon:
			bodies.Symbol(token.Value)
		}
		switch token.Value {
		case "(", "[", "{":
			closers = append(closers, map[string]string{"(": ")", "[": "]", "{": "}"}[token.Value])
		case ")", "]", "}":
			if len(closers) == 0 || closers[len(closers)-1] != token.Value {
				p.failf("unbalanced query body")
			} else {
				closers = closers[:len(closers)-1]
			}
		}
		end = token.End
		p.pos++
	}
	if start == end || len(closers) != 0 || keepSemicolon(&bodies) {
		p.failf("expected a complete query body")
	}
	return strings.TrimSpace(p.text[start:end])
}

// keepSemicolon reports whether a semicolon read now is body text rather
// than the end of the statement. It advances the state, as a semicolon
// separates the words on either side of it.
func keepSemicolon(bodies *yql.Bodies) bool {
	bodies.Separate()
	return bodies.Inside()
}
