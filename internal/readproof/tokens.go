// Package readproof proves that an operator's YQL query reads only local
// database objects before it runs in a snapshot read-only transaction.
package readproof

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// MaxNesting bounds the chain of views the proof follows.
const MaxNesting = 4

var sourceListEnds = []string{"WHERE", "GROUP", "HAVING", "ORDER", "LIMIT", "OFFSET", "WINDOW", "UNION", "INTERSECT", "EXCEPT", "INTO", "SELECT", "ASSUME", "PRESORT"}

func yqlTokens(text string) []yql.Token {
	return slices.DeleteFunc(yql.Tokens(text), func(token yql.Token) bool {
		return token.Type == yql.TokenWhitespace || token.Type == yql.TokenComment
	})
}

func isKeyword(token yql.Token, word string) bool { return token.IsWord(word) }

// localCalls refuses functions that read files, secrets, code or PostgreSQL
// functions. Namespace calls and cluster prefixes are refused by ReadOnly's
// colon check. The names are the YQL builtins measured by the host's read proof.
func localCalls(tokens []yql.Token) error {
	for i, token := range tokens {
		if i+1 == len(tokens) || !tokens[i+1].IsOperator("(") || token.Type != yql.TokenIdentifier {
			continue
		}
		name := strings.ToUpper(strings.Trim(token.Value, "`"))
		if slices.Contains([]string{"FILECONTENT", "FILEPATH", "FILES", "FOLDERPATH", "PARSEFILE", "FOLDER", "WALKFOLDERS", "SECUREPARAM", "UDF", "SCRIPTUDF", "WITHSIDEEFFECTS", "WITHSIDEEFFECTSMODE"}, name) ||
			strings.HasPrefix(name, "PG") || strings.HasPrefix(name, "EVALUATE") || strings.HasSuffix(name, "CODE") {
			return fmt.Errorf("the query calls %s, whose effects cannot be proved local to the database", name)
		}
	}
	return nil
}
