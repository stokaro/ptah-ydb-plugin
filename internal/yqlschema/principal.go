package yqlschema

import (
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbacl"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// userRefusal is the host's refusal of a malformed user declaration, which
// names no token: a password can sit where a name, an option or a delimiter
// was expected.
const userRefusal = "invalid user declaration; expected a user name and PASSWORD, HASH, LOGIN or NOLOGIN options"

// userError hides every token in a user declaration error.
func (p *parser) userError() {
	if !p.done() && p.peek().Type != yql.TokenSemicolon {
		p.failf("expected ';' after the declaration")
	}
	p.failAt(userRefusal)
}

func (p *parser) createUser() *createRole {
	defer p.userError()
	node := &createRole{name: decodedName(p.identifier()), login: true}
	p.checkPrincipalName(node.name)
	for _, option := range p.userOptions() {
		if option.password != nil {
			node.password = option.password
		}
		if option.login != nil {
			node.login = *option.login
		}
	}
	return node
}

func (p *parser) alterUser() *alterUser {
	defer p.userError()
	node := &alterUser{name: decodedName(p.identifier())}
	p.checkPrincipalName(node.name)
	if p.word("WITH") {
		p.pos++
	}
	node.options = p.userOptions()
	if len(node.options) == 0 {
		p.failf("ALTER USER needs an option")
	}
	return node
}

func (p *parser) checkPrincipalName(name string) {
	if err := ydbacl.CheckName(name); err != nil {
		p.failf("%s", err)
	}
}

func (p *parser) userOptions() []roleOption {
	var options []roleOption
	var passwordSeen, loginSeen bool
	for !p.done() && p.peek().Type != yql.TokenSemicolon {
		switch {
		case p.word("PASSWORD"), p.word("HASH"):
			clause := "PASSWORD"
			if p.word("HASH") {
				clause = "HASH"
			}
			if passwordSeen {
				p.failf("duplicate password option")
			}
			passwordSeen = true
			p.pos++
			password := p.userPassword(clause)
			options = append(options, roleOption{password: &password})
		case p.word("LOGIN"), p.word("NOLOGIN"):
			if loginSeen {
				p.failf("duplicate login option")
			}
			loginSeen = true
			login := p.word("LOGIN")
			options = append(options, roleOption{login: &login})
			p.pos++
		default:
			p.failf("unsupported user option")
		}
	}
	return options
}

func (p *parser) userPassword(clause string) string {
	if clause == "PASSWORD" && p.word("NULL") {
		p.pos++
		return ""
	}
	value, ok := stringLiteral(p.peek().Value, "String")
	if !ok || (clause == "HASH") != ydbacl.IsPasswordHash(value) {
		p.failf("expected a password literal or a YDB password hash")
	}
	p.pos++
	return value
}

func (p *parser) createGroup() []statement {
	name := decodedName(p.identifier())
	p.checkPrincipalName(name)
	result := []statement{&createRole{name: name, group: true}}
	if p.word("WITH") {
		p.pos++
		p.wantWord("USER")
		result = append(result, p.groupMembers(name, "ADD")...)
	}
	return result
}

func (p *parser) alterGroup() []statement {
	name := decodedName(p.identifier())
	clause := "ADD"
	if p.word("DROP") {
		clause = "DROP"
	}
	if !p.word("ADD") && !p.word("DROP") {
		p.failf("expected ADD USER or DROP USER")
	}
	p.pos++
	p.wantWord("USER")
	return p.groupMembers(name, clause)
}

func (p *parser) groupMembers(group, clause string) []statement {
	var result []statement
	for !p.done() {
		member := decodedName(p.identifier())
		result = append(result, &membership{group: group, member: member, add: clause == "ADD"})
		if !p.accept(",") {
			break
		}
		if p.done() {
			p.failf("expected a group member after comma")
		}
	}
	if len(result) == 0 {
		p.failf("expected a group member")
	}
	return result
}
