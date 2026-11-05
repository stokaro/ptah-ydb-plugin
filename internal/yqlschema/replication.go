package yqlschema

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/replication"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// createReplication is a CREATE ASYNC REPLICATION.
type createReplication struct {
	spanned
	schema, name string
	spec         replication.ReplicationSpec
}

// createTransfer is a CREATE TRANSFER.
type createTransfer struct {
	spanned
	schema, name string
	spec         replication.TransferSpec
}

// replicationSettings is an ALTER ASYNC REPLICATION ... SET or an ALTER
// TRANSFER ... SET: the settings it names, which conversion folds into the
// object an earlier statement of the load declared. A transfer's names a new
// lambda under using.
type replicationSettings struct {
	spanned
	kind         replication.SourceKind
	schema, name string
	settings     map[string]string
}

// replicationError replaces any refusal of an async replication or a
// transfer, the statement's end included, with one that names no token: a
// connection clause may hold a credential written by mistake.
func (p *parser) replicationError() {
	if !p.done() && p.peek().Type != yql.TokenSemicolon {
		p.failf("expected ';'")
	}
	p.failAt(replicationRefusal)
}

// replicationName reads the path of an async replication or a transfer: a
// directory and a name, relative to the database root.
func (p *parser) replicationName() (directory, name string) {
	ref, err := ydbdecl.ParseTopicPath(decodedName(p.path()))
	if err != nil {
		p.failf("invalid path")
		return "", ""
	}
	return ref.Schema, ref.Name
}

// createReplication reads `CREATE ASYNC REPLICATION <path> FOR <source> AS
// <target>, ... WITH (...)`, after ASYNC.
func (p *parser) createReplication() *createReplication {
	defer p.replicationError()
	p.wantWord("REPLICATION")
	schema, name := p.replicationName()
	p.wantWord("FOR")
	var items []replication.Item
	for !p.done() {
		source := p.replicationPath(p.identifier())
		p.wantWord("AS")
		target := p.replicationPath(p.path())
		item, err := replication.ParseItem(map[string]string{replication.AttributeSource: source, replication.AttributeTarget: target})
		if err != nil {
			p.failf("invalid replication item")
		}
		items = append(items, item)
		if !p.accept(",") {
			break
		}
	}
	p.wantWord("WITH")
	spec, err := replication.ParseReplicationSource(p.replicationSettings(replication.ReplicationSource))
	if err != nil {
		p.failf("invalid replication settings")
	}
	spec.Items = items
	if err := replication.ValidateReplicationItems(items); err != nil {
		p.failf("invalid replication items")
	}
	return &createReplication{schema: schema, name: name, spec: spec}
}

// createTransfer reads `CREATE TRANSFER <path> FROM <topic> TO <table> USING
// <lambda> [WITH (...)]`, after TRANSFER.
func (p *parser) createTransfer() *createTransfer {
	defer p.replicationError()
	schema, name := p.replicationName()
	p.wantWord("FROM")
	source := p.replicationPath(p.identifier())
	p.wantWord("TO")
	target := p.replicationPath(p.path())
	p.wantWord("USING")
	lambda := p.transferLambda()
	settings := make(map[string]string)
	if p.word("WITH") {
		p.pos++
		settings = p.replicationSettings(replication.TransferSource)
	}
	settings[replication.AttributeSource], settings[replication.AttributeTarget], settings[replication.AttributeUsing] = source, target, lambda
	spec, err := replication.ParseTransferSource(settings)
	if err != nil {
		p.failf("invalid transfer settings")
	}
	return &createTransfer{schema: schema, name: name, spec: spec}
}

// alterReplication reads `ALTER ASYNC REPLICATION <path> SET (...)`, after
// ASYNC.
func (p *parser) alterReplication() *replicationSettings {
	defer p.replicationError()
	p.wantWord("REPLICATION")
	schema, name := p.replicationName()
	p.wantWord("SET")
	return &replicationSettings{kind: replication.ReplicationSource, schema: schema, name: name, settings: p.replicationSettings(replication.ReplicationSource)}
}

// alterTransfer reads `ALTER TRANSFER <path> SET USING <lambda>, SET (...)`,
// either clause alone or both, after TRANSFER.
func (p *parser) alterTransfer() *replicationSettings {
	defer p.replicationError()
	schema, name := p.replicationName()
	node := &replicationSettings{kind: replication.TransferSource, schema: schema, name: name, settings: make(map[string]string)}
	for !p.done() {
		p.wantWord("SET")
		settings := make(map[string]string)
		if p.word("USING") {
			p.pos++
			settings[replication.AttributeUsing] = p.transferLambda()
		} else {
			settings = p.replicationSettings(replication.TransferSource)
		}
		for name, value := range settings {
			if _, exists := node.settings[name]; exists {
				p.failf("duplicate transfer setting")
			}
			node.settings[name] = value
		}
		if !p.accept(",") {
			break
		}
	}
	return node
}

// replicationSettings reads a WITH (...) or SET (...) of kind, each value in
// the form [replication.SourceSettingType] names.
func (p *parser) replicationSettings(kind replication.SourceKind) map[string]string {
	return p.optionsUsing(func(key string) string {
		raw := p.expression()
		switch replication.SourceSettingType(key, kind) {
		case "String":
			value, ok := stringLiteral(raw, "String")
			if !ok || value == "" || value != strings.TrimSpace(value) {
				p.failf("expected a nonempty string literal")
			}
			return value
		case "Uint":
			value, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || value == 0 {
				p.failf("expected a positive integer")
			}
			return raw
		case "Interval":
			literal := newParser(raw)
			literal.wantWord("Interval")
			literal.want("(")
			value, ok := stringLiteral(literal.expression(), "String")
			literal.want(")")
			if !ok || value != strings.TrimSpace(value) || literal.err != nil || !literal.done() {
				p.failf("expected an interval literal")
			}
			return value
		default:
			p.failf("unsupported replication or transfer setting")
			return ""
		}
	})
}

// transferLambda reads an inline lambda, `($msg) -> { ... }`, and keeps it
// byte for byte, nested blocks and semicolons included. Only its grammar
// boundary is read; the server checks its expressions.
func (p *parser) transferLambda() string {
	start := p.peek().Start
	p.want("(")
	for !p.done() && p.peek().Value != ")" {
		token := p.peek()
		if token.Type != yql.TokenIdentifier || !strings.HasPrefix(token.Value, "$") {
			p.failf("expected a lambda parameter")
		}
		p.pos++
		if !p.accept(",") {
			break
		}
	}
	p.want(")")
	if !p.accept("->") {
		p.want("-")
		p.want(">")
	}
	p.want("{")
	closers := []string{"}"}
	end := p.peek().Start
	for !p.done() && len(closers) > 0 {
		token := p.peek()
		if token.Type == yql.TokenUnknown {
			p.failf("unrecognized lambda token")
			break
		}
		switch token.Value {
		case "(", "[", "{":
			closers = append(closers, map[string]string{"(": ")", "[": "]", "{": "}"}[token.Value])
		case ")", "]", "}":
			if token.Value != closers[len(closers)-1] {
				p.failf("unbalanced lambda")
			} else {
				closers = closers[:len(closers)-1]
			}
		}
		end = token.End
		p.pos++
	}
	if len(closers) != 0 {
		p.failf("incomplete lambda")
	}
	return strings.TrimSpace(p.text[start:end])
}

// replicationPath is a written path as YDB resolves it. Unlike a source
// annotation's value, a quoted YQL path is never trimmed: whitespace changes
// its name.
func (p *parser) replicationPath(name string) string {
	value := decodedName(name)
	if value != strings.TrimSpace(value) {
		p.failf("path has surrounding whitespace")
	}
	return value
}

// declareReplication declares the async replication a CREATE names.
func (c *converter) declareReplication(node *createReplication) error {
	object := Object{Kind: KindReplication, Schema: node.schema, Name: node.name, Spec: new(node.spec)}
	if c.file.object(object) != nil {
		return fmt.Errorf("async replication %s is declared twice", ydbdecl.TopicDisplay(node.schema, node.name))
	}
	return c.declare(object)
}

// declareTransfer declares the transfer a CREATE names.
func (c *converter) declareTransfer(node *createTransfer) error {
	object := Object{Kind: KindTransfer, Schema: node.schema, Name: node.name, Spec: new(node.spec)}
	if c.file.object(object) != nil {
		return fmt.Errorf("transfer %s is declared twice", ydbdecl.TopicDisplay(node.schema, node.name))
	}
	return c.declare(object)
}

// applyReplicationSettings folds an ALTER into the object this document, or
// one before it, declares at the path.
func (c *converter) applyReplicationSettings(node *replicationSettings) error {
	kind, noun := KindReplication, "ALTER ASYNC REPLICATION"
	if node.kind == replication.TransferSource {
		kind, noun = KindTransfer, "ALTER TRANSFER"
	}
	for _, reading := range c.readings() {
		held := reading.object(Object{Kind: kind, Schema: node.schema, Name: node.name})
		if held == nil {
			continue
		}
		switch spec := held.Spec.(type) {
		case *replication.ReplicationSpec:
			changed, err := replication.ApplyReplicationSettings(*spec, node.settings)
			if err != nil {
				return fmt.Errorf("%w: invalid async replication settings change", errUnmodeled)
			}
			held.Spec = &changed
		case *replication.TransferSpec:
			changed, err := replication.ApplyTransferSettings(*spec, node.settings)
			if err != nil {
				return fmt.Errorf("%w: invalid transfer settings change", errUnmodeled)
			}
			held.Spec = &changed
		}
		return nil
	}
	return fmt.Errorf("%w: %s needs an earlier declaration", errUnmodeled, noun)
}
