package yqlschema

import (
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// splitPoints reads native literals before the declaration codec sees them.
// The codec holds values, while YQL strings carry escapes and a type suffix
// that must not become part of the partition boundary.
func splitPoints(text string) ([][]string, error) {
	p := newParser(text)
	p.want("(")
	var points [][]string
	for !p.done() {
		tuple := p.accept("(")
		point := []string{p.splitValue()}
		if tuple {
			for p.accept(",") {
				point = append(point, p.splitValue())
			}
			p.want(")")
		}
		points = append(points, point)
		if !p.accept(",") {
			break
		}
	}
	p.want(")")
	if !p.done() {
		p.failf("unexpected text after partition boundaries")
	}
	if p.err != nil {
		return points, p.err
	}
	return points, nil
}

func (p *parser) splitValue() string {
	token := p.peek()
	if token.Type == yql.TokenString {
		columnType := ydbtype.String
		if strings.HasSuffix(strings.ToLower(token.Value), "u") {
			columnType = ydbtype.Utf8
		}
		value, ok := stringLiteral(token.Value, columnType)
		if !ok {
			p.failf("invalid string partition boundary")
		}
		p.pos++
		return value
	}
	if token.Value == "" || strings.Trim(token.Value, "0123456789") != "" {
		p.failf("partition boundary must be a nonnegative integer or string literal")
		return ""
	}
	p.pos++
	return token.Value
}
