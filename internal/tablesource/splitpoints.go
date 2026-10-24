package tablesource

import (
	"errors"
	"fmt"
	"strings"
)

// parseSplitPoints reads the split points of partition_at_keys: a comma
// separated list whose items are a value, for a split on the first key column,
// or a parenthesized list of values, for a split on the leading key columns.
// `10, 20, 30` is three split points of one value each, and `(10, 'a'), (20)`
// is two, the first on two key columns. A value is a run of characters with no
// space, comma, parenthesis or quote in it, or a string in single or double
// quotes, in which a backslash escapes the next character.
//
// The grammar is YQL's own list, so a declaration reads like the setting it
// becomes. It holds only values, never an expression. The bundled decoder in
// the host reads the same grammar, so a declaration means the same to both.
func parseSplitPoints(text string) ([][]string, error) {
	p := splitPointParser{text: text}
	p.skipSpace()
	if p.done() {
		return nil, errors.New("name at least one split point, such as 10, 20 or (10, 'a'), (20)")
	}
	var points [][]string
	for {
		p.skipSpace()
		point, err := p.point()
		if err != nil {
			return nil, err
		}
		points = append(points, point)
		p.skipSpace()
		if p.done() {
			return points, nil
		}
		if !p.take(',') {
			return nil, fmt.Errorf("expected a comma after split point %d at offset %d", len(points), p.at)
		}
	}
}

// splitPointParser reads the grammar of parseSplitPoints.
type splitPointParser struct {
	text string
	at   int
}

func (p *splitPointParser) done() bool { return p.at >= len(p.text) }

func (p *splitPointParser) skipSpace() {
	for !p.done() && strings.ContainsRune(" \t\r\n", rune(p.text[p.at])) {
		p.at++
	}
}

func (p *splitPointParser) take(c byte) bool {
	if !p.done() && p.text[p.at] == c {
		p.at++
		return true
	}
	return false
}

// point reads one split point: a value, or a parenthesized list of them.
func (p *splitPointParser) point() ([]string, error) {
	if !p.take('(') {
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		return []string{value}, nil
	}
	var values []string
	for {
		p.skipSpace()
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		p.skipSpace()
		if p.take(')') {
			return values, nil
		}
		if !p.take(',') {
			return nil, fmt.Errorf("expected a comma or a closing parenthesis at offset %d", p.at)
		}
	}
}

// value reads one value, quoted or bare.
func (p *splitPointParser) value() (string, error) {
	if p.done() {
		return "", errors.New("a split point ends without a value")
	}
	if quote := p.text[p.at]; quote == '\'' || quote == '"' {
		p.at++
		var b strings.Builder
		for !p.done() {
			c := p.text[p.at]
			p.at++
			switch {
			case c == quote:
				return b.String(), nil
			case c == '\\' && !p.done():
				b.WriteByte(p.text[p.at])
				p.at++
			default:
				b.WriteByte(c)
			}
		}
		return "", fmt.Errorf("a string value has no closing %c", quote)
	}
	start := p.at
	for !p.done() && !strings.ContainsRune(" \t\r\n,()'\"", rune(p.text[p.at])) {
		p.at++
	}
	if p.at == start {
		return "", fmt.Errorf("expected a value at offset %d", start)
	}
	return p.text[start:p.at], nil
}
