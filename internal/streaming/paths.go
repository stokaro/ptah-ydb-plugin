package streaming

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// pathBinding distinguishes a scalar path from a relation. Arbitrary scalar
// expressions cannot prove a path, even when their text contains string literals.
type pathBinding struct {
	scalar   string
	constant bool
	relation bool
	list     []string
}

// NamedPaths returns the exact local paths a query reads or writes. Constant
// named paths, named subqueries, AS_TABLE, CONCAT and EACH retain their static
// dependencies. Qualified references depend on the local external data source.
// Dynamic table paths and functions whose complete input set is unknown refuse.
func NamedPaths(text string) ([]string, error) {
	tokens := bodyTokens(text)
	bindings := make(map[string]pathBinding)
	implicit := make(map[int]bool)
	var paths []string
	prefix := ""
	for index, token := range tokens {
		if token.IsWord("USE") {
			return nil, fmt.Errorf("streaming query USE requires an explicitly bound data source")
		}
		if (index == 0 || tokens[index-1].Type == yql.TokenSemicolon) && strings.HasPrefix(token.Value, "$") && index+1 < len(tokens) && tokens[index+1].IsOperator("=") {
			end := index + 2
			for end < len(tokens) && tokens[end].Type != yql.TokenSemicolon {
				end++
			}
			bindings[token.Value] = bindPath(tokens[index+2:end], bindings)
		}
		if token.IsWord("PRAGMA") && index+1 < len(tokens) && tokens[index+1].IsWord("TablePathPrefix") {
			if index+4 >= len(tokens) || !tokens[index+2].IsOperator("=") || tokens[index+4].Type != yql.TokenSemicolon {
				return nil, fmt.Errorf("streaming query TablePathPrefix requires one constant string")
			}
			value, ok := yql.StringValue(tokens[index+3].Value)
			if !ok || !validPath(value, true) {
				return nil, fmt.Errorf("streaming query TablePathPrefix requires a clean path string")
			}
			prefix = value
		}
		if token.IsWord("FROM") {
			for _, operand := range commaSources(tokens, index+1) {
				implicit[operand-1] = true
			}
		}
		if index+1 >= len(tokens) || (!token.IsWord("FROM") && !token.IsWord("JOIN") && !token.IsWord("INTO") && !implicit[index]) {
			continue
		}
		names, err := sourcePaths(tokens[index+1:], bindings, token.IsWord("INTO"))
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if !validPath(name, false) {
				return nil, fmt.Errorf("streaming query contains an invalid scheme path")
			}
			if prefix != "" && !path.IsAbs(name) {
				name = path.Join(prefix, name)
			}
			if !slices.Contains(paths, name) {
				paths = append(paths, name)
			}
		}
	}
	return paths, nil
}

// commaSources reads implicit cross joins, enabled by AnsiImplicitCrossJoin.
// Commas inside expressions, subqueries, lists and source settings are not
// source separators. The enclosing SELECT's next clause ends the source list.
func commaSources(tokens []yql.Token, start int) []int {
	var result []int
	depth := 0
	for index := start; index < len(tokens); index++ {
		token := tokens[index]
		if depth == 0 {
			if token.Type == yql.TokenSemicolon || token.IsOperator(")") || token.IsOperator("}") ||
				token.IsWord("WHERE") || token.IsWord("GROUP") || token.IsWord("HAVING") || token.IsWord("ORDER") ||
				token.IsWord("LIMIT") || token.IsWord("UNION") || token.IsWord("WINDOW") || token.IsWord("QUALIFY") ||
				token.IsWord("SELECT") || token.IsWord("FLATTEN") {
				return result
			}
			if token.IsOperator(",") && index+1 < len(tokens) {
				result = append(result, index+1)
			}
		}
		switch {
		case token.IsOperator("("), token.IsOperator("["), token.IsOperator("{"):
			depth++
		case token.IsOperator(")"), token.IsOperator("]"), token.IsOperator("}"):
			depth--
		}
	}
	return result
}

func bindPath(tokens []yql.Token, bindings map[string]pathBinding) pathBinding {
	for len(tokens) >= 2 && tokens[0].IsOperator("(") && closingParen(tokens) == len(tokens)-1 {
		tokens = tokens[1 : len(tokens)-1]
	}
	if len(tokens) == 0 {
		return pathBinding{}
	}
	if tokens[0].IsWord("SELECT") || tokens[0].IsWord("FROM") || tokens[0].IsWord("AS_TABLE") {
		return pathBinding{relation: true}
	}
	if len(tokens) == 1 {
		if tokens[0].Type == yql.TokenString {
			value, ok := yql.StringValue(tokens[0].Value)
			return pathBinding{scalar: value, constant: ok}
		}
		return bindings[tokens[0].Value]
	}
	if tokens[0].IsOperator("[") && tokens[len(tokens)-1].IsOperator("]") {
		result := pathBinding{list: []string{}}
		for _, argument := range pathArguments(tokens[1 : len(tokens)-1]) {
			value := bindPath(argument, bindings)
			if !value.constant {
				return pathBinding{}
			}
			result.list = append(result.list, value.scalar)
		}
		return result
	}
	// Concatenation of known strings is still a constant; other expressions
	// remain unresolved instead of harvesting incidental literals as paths.
	for index, token := range tokens {
		if token.IsOperator("|") && index+1 < len(tokens) && tokens[index+1].IsOperator("|") {
			left, right := bindPath(tokens[:index], bindings), bindPath(tokens[index+2:], bindings)
			if left.constant && right.constant {
				return pathBinding{scalar: left.scalar + right.scalar, constant: true}
			}
			break
		}
	}
	return pathBinding{}
}

func sourcePaths(tokens []yql.Token, bindings map[string]pathBinding, target bool) ([]string, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("streaming query requires a statically bound source or target")
	}
	name := tokens[0]
	if name.IsOperator("(") {
		end := closingParen(tokens)
		if !target && end > 0 && bindPath(tokens[:end+1], bindings).relation {
			return nil, nil // The outer walk also visits the subquery's sources.
		}
		return nil, fmt.Errorf("streaming query contains an unresolved table expression")
	}
	if len(tokens) > 1 && tokens[1].IsOperator(".") {
		if len(tokens) < 3 || tokens[2].Type != yql.TokenIdentifier || len(tokens) > 3 && (tokens[3].IsOperator("(") || tokens[3].IsOperator(".")) {
			return nil, fmt.Errorf("streaming query contains an unresolved qualified data source")
		}
		return literalPath(name, bindings, false)
	}
	if len(tokens) > 1 && tokens[1].IsOperator("(") {
		if target {
			return nil, fmt.Errorf("streaming query target must name one static path")
		}
		end := closingParen(tokens[1:])
		if end < 0 {
			return nil, fmt.Errorf("streaming query contains an incomplete table function")
		}
		if name.IsWord("AS_TABLE") {
			return nil, nil // AS_TABLE consumes values; it does not open scheme paths.
		}
		arguments := pathArguments(tokens[2 : end+1])
		if name.IsWord("EACH") && len(arguments) == 1 {
			value := bindPath(arguments[0], bindings)
			if value.list != nil {
				return value.list, nil
			}
		}
		if name.IsWord("CONCAT") || name.IsWord("CONCAT_STRICT") {
			var result []string
			for _, argument := range arguments {
				if bound := bindPath(argument, bindings); bound.constant {
					result = append(result, bound.scalar)
					continue
				}
				if len(argument) != 1 {
					return nil, fmt.Errorf("streaming query CONCAT requires static table paths")
				}
				paths, err := literalPath(argument[0], bindings, false)
				if err != nil {
					return nil, err
				}
				result = append(result, paths...)
			}
			return result, nil
		}
		return nil, fmt.Errorf("streaming query table function %q has unresolved physical inputs", name.Value)
	}
	return literalPath(name, bindings, !target)
}

func literalPath(token yql.Token, bindings map[string]pathBinding, relation bool) ([]string, error) {
	if strings.HasPrefix(token.Value, "$") {
		bound := bindings[token.Value]
		if bound.constant {
			return []string{bound.scalar}, nil
		}
		if relation && bound.relation {
			return nil, nil
		}
		return nil, fmt.Errorf("streaming query named table %q has no static path or subquery binding", token.Value)
	}
	if token.Type == yql.TokenString {
		if value, ok := yql.StringValue(token.Value); ok {
			return []string{value}, nil
		}
	}
	if token.Type == yql.TokenIdentifier {
		if value, ok := yql.IdentifierValue(token.Value); ok {
			return []string{value}, nil
		}
		return []string{token.Value}, nil
	}
	return nil, fmt.Errorf("streaming query contains an unresolved source or target")
}

func closingParen(tokens []yql.Token) int {
	depth := 0
	for index, token := range tokens {
		if token.IsOperator("(") {
			depth++
		}
		if token.IsOperator(")") {
			depth--
			if depth == 0 {
				return index
			}
		}
	}
	return -1
}

func pathArguments(tokens []yql.Token) [][]yql.Token {
	var result [][]yql.Token
	start, depth := 0, 0
	for index, token := range tokens {
		switch {
		case token.IsOperator("("), token.IsOperator("["):
			depth++
		case token.IsOperator(")"), token.IsOperator("]"):
			depth--
		case token.IsOperator(",") && depth == 0:
			result = append(result, tokens[start:index])
			start = index + 1
		}
	}
	if start < len(tokens) {
		result = append(result, tokens[start:])
	}
	return result
}
