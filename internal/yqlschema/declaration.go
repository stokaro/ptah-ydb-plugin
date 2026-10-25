package yqlschema

import (
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

func (p *parser) declaration() []statement {
	if p.word("GRANT") || p.word("REVOKE") {
		return p.privilegeStatement()
	}
	if p.word("ALTER") {
		return p.alterDeclaration()
	}
	if p.word("COMMENT") {
		return one(p.comment())
	}
	p.wantWord("CREATE")
	return p.createDeclaration()
}

// one is a declaration that is one statement.
func one[T statement](node T) []statement {
	return []statement{node}
}

func (p *parser) createDeclaration() []statement {
	switch {
	case p.word("ASYNC"):
		return p.unsupportedReplication("CREATE ASYNC REPLICATION", "ASYNC", "REPLICATION")
	case p.word("TRANSFER"):
		return p.unsupportedReplication("CREATE TRANSFER", "TRANSFER")
	case p.word("STREAMING"):
		return p.unsupported("CREATE STREAMING QUERY", "STREAMING", "QUERY")
	case p.word("USER"):
		p.pos++
		return one(p.createUser())
	case p.word("GROUP"):
		p.pos++
		return p.createGroup()
	case p.word("TABLE"):
		p.pos++
		return one(p.table())
	case p.word("VIEW"):
		p.pos++
		return one(p.view())
	case p.word("TOPIC"):
		p.pos++
		return one(p.topic())
	case p.word("COORDINATION"):
		p.pos++
		return one(p.coordination())
	case p.word("SECRET"):
		return p.unsupported("CREATE SECRET", "SECRET")
	case p.word("EXTERNAL"):
		p.pos++
		return p.external()
	case p.word("OR"):
		p.pos++
		return p.replacementDeclaration()
	case p.word("RESOURCE"):
		if p.pos+2 < len(p.tokens) && p.tokens[p.pos+1].IsWord("POOL") && p.tokens[p.pos+2].IsWord("CLASSIFIER") {
			return p.unsupported("CREATE RESOURCE POOL CLASSIFIER", "RESOURCE", "POOL", "CLASSIFIER")
		}
		return p.unsupported("CREATE RESOURCE POOL", "RESOURCE", "POOL")
	default:
		p.failf("this CREATE object kind is not supported in a desired YQL schema")
		return nil
	}
}

func (p *parser) replacementDeclaration() []statement {
	p.wantWord("REPLACE")
	if p.word("STREAMING") {
		return p.unsupported("CREATE OR REPLACE STREAMING QUERY", "STREAMING", "QUERY")
	}
	p.wantWord("EXTERNAL")
	return p.external()
}

func (p *parser) alterDeclaration() []statement {
	p.wantWord("ALTER")
	switch {
	case p.word("SEQUENCE"):
		p.pos++
		return one(p.alterSerialSequence())
	case p.word("ASYNC"):
		return p.unsupportedReplication("ALTER ASYNC REPLICATION", "ASYNC", "REPLICATION")
	case p.word("TRANSFER"):
		return p.unsupportedReplication("ALTER TRANSFER", "TRANSFER")
	case p.word("USER"):
		p.pos++
		return one(p.alterUser())
	case p.word("GROUP"):
		p.pos++
		return p.alterGroup()
	case p.word("TABLE"):
		p.pos++
		return one(p.changefeed())
	case p.word("TOPIC"):
		p.pos++
		ref, err := ydbdecl.ParseTopicPath(decodedName(p.path()))
		if err != nil {
			p.failf("%v", err)
		}
		p.wantWord("ADD")
		p.wantWord("CONSUMER")
		return one(&addConsumer{schema: ref.Schema, name: ref.Name, consumer: p.consumer()})
	case p.word("RESOURCE"):
		return p.unsupported("ALTER RESOURCE POOL", "RESOURCE", "POOL")
	default:
		// The host's reader reads any other ALTER as the server-owned
		// default resource pool's, and refuses it unless it spells one.
		p.wantWord("RESOURCE")
		return nil
	}
}

// unsupportedReplication is [parser.unsupported] for an async replication
// or a transfer, whose keywords the host's reader refuses without naming a
// token, since a connection clause may hold a credential.
func (p *parser) unsupportedReplication(construct string, keywords ...string) []statement {
	for _, keyword := range keywords {
		if !p.word(keyword) {
			p.failf("expected %s", keyword)
			p.failAt(replicationRefusal)
			return nil
		}
		p.pos++
	}
	return p.skip(construct)
}

// replicationRefusal is the host's refusal of a malformed async replication
// or transfer.
const replicationRefusal = "invalid replication or transfer declaration; use modeled settings and secret references, with an inline lambda for a transfer"

// unsupported reads a statement the host's reader declares an object of and
// this provider has no model for: the keywords that name its kind, which the
// host's reader requires as they are, and then everything to the semicolon
// that ends it. It returns the statement to be refused by name.
func (p *parser) unsupported(construct string, keywords ...string) []statement {
	for _, keyword := range keywords {
		p.wantWord(keyword)
	}
	if p.err != nil {
		return nil
	}
	return p.skip(construct)
}

// skip consumes the rest of a statement. A semicolon inside an inline action
// or a lambda belongs to the statement, as it does to the view a CREATE VIEW
// ... AS DO declares. No token of the statement is ever reported: a secret, a
// password and a replication's connection all live in these statements.
func (p *parser) skip(construct string) []statement {
	var bodies yql.Bodies
	depth := 0
	for !p.done() {
		token := p.peek()
		if token.Type == yql.TokenSemicolon && !keepSemicolon(&bodies) && depth == 0 {
			break
		}
		switch {
		case token.Type == yql.TokenIdentifier:
			bodies.Word(strings.ToUpper(token.Value))
		case token.Type != yql.TokenSemicolon:
			bodies.Symbol(token.Value)
		}
		switch token.Value {
		case "(", "[":
			depth++
		case ")", "]":
			if depth > 0 {
				depth--
			}
		}
		p.pos++
	}
	return []statement{&unsupported{construct: construct}}
}
