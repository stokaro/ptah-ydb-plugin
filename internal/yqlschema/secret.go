package yqlschema

import (
	"github.com/stokaro/ptah-ydb-plugin/internal/secrets"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	"strings"
)

// secret accepts a reference only. Every parse error is replaced because a
// malformed declaration can put a credential in any token, even an option name.
func (p *parser) secret() *createSecret {
	defer func() {
		p.failAt("invalid CREATE SECRET declaration; use a database-relative path and WITH (value = $PTAH_SECRET_<name>); literal values and other options are not supported")
	}()
	ref, err := ydbdecl.ParseTopicPath(decodedName(p.path()))
	if err != nil {
		p.failf("invalid secret path")
	}
	p.wantWord("WITH")
	p.want("(")
	p.wantWord("value")
	p.want("=")
	token := p.peek()
	variable, prefixed := strings.CutPrefix(token.Value, "$")
	if token.Type != yql.TokenIdentifier || !prefixed {
		p.failf("a secret value must name its input")
	}
	if err := secrets.CheckValueEnv(variable); err != nil {
		p.failf("invalid secret input")
	}
	if !p.done() {
		p.pos++
	}
	p.want(")")
	if !p.done() && p.peek().Type != yql.TokenSemicolon {
		p.failf("expected ';' after the declaration")
	}
	return &createSecret{schema: ref.Schema, name: ref.Name, spec: secrets.Spec{ValueEnv: variable}}
}
