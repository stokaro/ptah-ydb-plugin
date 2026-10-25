package yqlschema

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// parser reads one YQL schema document, or one value inside it, from the
// tokens internal/yql reads. It stops at the first error, as the host's
// internal/yqlparse does, and every later step returns without consuming.
type parser struct {
	text   string
	tokens []yql.Token
	pos    int
	err    *syntaxError
}

// syntaxError is a refusal of the text at an offset. reason is what the
// host's parser writes after the position, the token it got included, so
// [syntaxError.Error] spells the host's refusal byte for byte: an error read
// inside another value is part of the outer reason in that spelling.
type syntaxError struct {
	offset int
	reason string
}

func (e *syntaxError) Error() string {
	return fmt.Sprintf("YQL schema at position %d: %s", e.offset, e.reason)
}

// parse reads a YQL schema document. It never executes the document or
// connects to a database. Unknown syntax fails the whole read rather than
// losing objects.
func parse(text string) ([]statement, *syntaxError) {
	p := newParser(text)
	var result []statement
	for !p.done() {
		if p.accept(";") {
			continue
		}
		start := p.peek().Start
		nodes := p.declaration()
		if p.err != nil {
			break
		}
		for _, node := range nodes {
			node.place(span{start: start, end: p.lastEnd(start)})
		}
		result = append(result, nodes...)
		if !p.done() && !p.accept(";") {
			p.failf("expected ';' after the declaration")
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	return result, nil
}

// lastEnd is where the last consumed token ends, or start when the
// declaration consumed none.
func (p *parser) lastEnd(start int) int {
	if p.pos == 0 || p.pos > len(p.tokens) {
		return start
	}
	if end := p.tokens[p.pos-1].End; end > start {
		return end
	}
	return start
}

func (p *parser) done() bool { return p.err != nil || p.pos >= len(p.tokens) }

func (p *parser) peek() yql.Token {
	if p.pos >= len(p.tokens) {
		return yql.Token{Type: yql.TokenEOF, Start: len(p.text), End: len(p.text)}
	}
	return p.tokens[p.pos]
}

func (p *parser) word(value string) bool { return p.peek().IsWord(value) }

func (p *parser) accept(value string) bool {
	if p.done() || p.peek().Value != value {
		return false
	}
	p.pos++
	return true
}

func (p *parser) want(value string) {
	if !p.accept(value) {
		p.failf("expected %q", value)
	}
}

func (p *parser) wantWord(value string) {
	if p.word(value) {
		p.pos++
		return
	}
	p.failf("expected %s", value)
}

func (p *parser) failf(format string, args ...any) {
	if p.err == nil {
		p.err = &syntaxError{offset: p.peek().Start, reason: fmt.Sprintf("%s (got %q)", fmt.Sprintf(format, args...), boundToken(p.peek().Value))}
	}
}

// maxToken is the most of a token a refusal quotes. The host's reader quotes
// the whole token; a schema file can hold a secret in a string literal, so
// this provider quotes no more than its start.
const maxToken = 64

// boundToken is a token as a refusal quotes it: whole when it is short, and
// otherwise its first maxToken bytes, cut at a character, and an ellipsis.
func boundToken(token string) string {
	if len(token) <= maxToken {
		return token
	}
	cut := maxToken
	for cut > 0 && !utf8.RuneStart(token[cut]) {
		cut--
	}
	return token[:cut] + "..."
}

// failAt replaces the error, if any, with a refusal that names no token, at
// the token the parser stopped at. A statement that may carry a credential
// reports this way.
func (p *parser) failAt(reason string) {
	if p.err != nil {
		p.err = &syntaxError{offset: p.peek().Start, reason: reason}
	}
}

func (p *parser) identifier() string {
	token := p.peek()
	name, ok := yql.IdentifierValue(token.Value)
	if token.Type != yql.TokenIdentifier || !ok || name == "" || strings.HasPrefix(name, "$") {
		p.failf("expected an identifier")
		return ""
	}
	p.pos++
	return token.Value
}

func (p *parser) names() []string {
	p.want("(")
	var names []string
	for !p.done() {
		names = append(names, p.identifier())
		if !p.accept(",") {
			break
		}
	}
	p.want(")")
	return names
}

// expression takes a balanced value without consuming the delimiter. The stop
// words apply only at the outer level, never inside a literal or constructor.
func (p *parser) expression(stopWords ...string) string {
	start, end, depth := p.peek().Start, p.peek().Start, 0
	for !p.done() {
		token := p.peek()
		if depth == 0 && (token.Value == "," || token.Value == ")" || token.Type == yql.TokenSemicolon || p.anyWord(stopWords)) {
			break
		}
		if token.Value == "(" {
			depth++
		}
		if token.Value == ")" {
			depth--
		}
		if token.Type == yql.TokenUnknown {
			p.failf("unrecognized token in a value")
			break
		}
		end = token.End
		p.pos++
	}
	if depth != 0 || end == start {
		p.failf("expected a complete value")
	}
	return strings.TrimSpace(p.text[start:end])
}

func (p *parser) anyWord(words []string) bool {
	return slices.ContainsFunc(words, p.word)
}

// scalar is the text a literal stands for, or the value as written when it
// is no string literal.
func scalar(value string) string {
	if decoded, ok := yql.StringValue(value); ok {
		return decoded
	}
	return value
}

// decodedName is the name an identifier stands for, and "" for a backticked
// name YQL refuses.
func decodedName(value string) string { name, _ := yql.IdentifierValue(value); return name }

func (p *parser) options() map[string]string {
	return p.optionsUsing(func(string) string { return p.expression() })
}

func (p *parser) optionsUsing(valueOf func(string) string) map[string]string {
	p.want("(")
	values := make(map[string]string)
	for !p.done() {
		name := strings.ToLower(decodedName(p.identifier()))
		if _, exists := values[name]; exists {
			p.failf("setting %q is declared twice", name)
		}
		p.want("=")
		values[name] = valueOf(name)
		if !p.accept(",") {
			break
		}
	}
	p.want(")")
	return values
}

// newParser reads the tokens of text without whitespace and comments. The
// translation setting --!syntax_v1 at the head of the text is skipped, as
// the server reads the default syntax with or without it. An unterminated
// block comment fails the read at its start.
func newParser(text string) *parser {
	p := &parser{text: text}
	for _, token := range yql.Tokens(text) {
		if token.Type == yql.TokenUnknown && strings.TrimSpace(token.Value) == "--!syntax_v1" {
			continue
		}
		if token.Type == yql.TokenComment && strings.HasPrefix(token.Value, "/*") && !strings.HasSuffix(token.Value, "*/") {
			p.err = &syntaxError{offset: token.Start, reason: "unterminated comment"}
			break
		}
		if token.Type != yql.TokenWhitespace && token.Type != yql.TokenComment {
			p.tokens = append(p.tokens, token)
		}
	}
	return p
}
