// Package ydbview holds what YDB does with a view that the renderer, the
// reader and the preparation of a declaration have to agree on: the clause
// every CREATE VIEW carries, the form in which the server keeps the view's
// query, and the pragma a dev realm session puts in front of it.
//
// The renderer writes the query as the declaration spells it, the server
// stores its own form of it, and the host compares the prepared declaration
// with what the server reports. [QueryText] is that form, so a view applied
// once is not planned again on every run.
package ydbview

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SecurityClause is the WITH clause every YDB view is created with.
//
// YDB runs a view's query with the rights of the user who reads the view, and
// requires the view to say so. The host measured on 25.1.4.7 and 26.2.1.14
// that a CREATE VIEW without the clause and one with `security_invoker =
// FALSE` are both refused with `security_invoker option must be explicitly
// enabled`, and that the option name is case-sensitive. The server keeps no
// record of the clause: DescribeView returns the query alone.
const SecurityClause = "WITH (security_invoker = TRUE)"

// QueryText returns the text YDB stores for a view whose query is body.
//
// The server keeps the tokens of its own lexer and nothing else: comments are
// dropped, every token is separated from the next by one space, and the
// semicolons that end the statement are gone. Case and the contents of quoted
// names and literals are kept, a multi-line string with its line breaks.
// Measured through DescribeView on 25.1.4.7 and 26.2.1.14, both lines alike:
//
//	SELECT u>>1, a>=1, s||'x', ($x) -> { RETURN $x + 1; } -- c
//	-> SELECT u > > 1 , a >= 1 , s || 'x' , ( $ x ) -> { RETURN $ x + 1 ; }
//
//	CAST(a AS Int32?), <|k:1|>, Math::Pi(), d > -1.5e10, 3ut;;
//	-> CAST ( a AS Int32 ? ) , <| k : 1 |> , Math :: Pi ( ) , d > - 1.5e10 , 3ut
//
// So the tokens are the grammar's: an operator of two or three characters is
// one token, except `>>`, which the grammar reads as two, a number keeps its
// exponent and its suffix, and `$` is a token of its own. The function is
// idempotent, so the server's text reads back as itself.
func QueryText(body string) string {
	tokens := tokens(body)
	for len(tokens) > 0 && tokens[len(tokens)-1] == ";" {
		tokens = tokens[:len(tokens)-1]
	}
	return strings.Join(tokens, " ")
}

// RealmPrefix is the pragma a session on a dev realm puts in front of every
// query. The connection and the view reader share this spelling, because YDB
// stores the pragma in a view's query text, where the reader must recognize
// it.
func RealmPrefix(root string) string {
	return "PRAGMA TablePathPrefix(" + StringLiteral(root) + ");\n"
}

// RealmQueryText removes the pragma of the dev realm whose directory is root
// from the front of a stored view query. A realm's names already resolve
// inside it, so the view a realm session reads is the query without the
// pragma. Only that exact prefix is removed: a pragma the query's author
// wrote, or one naming another directory, stays.
func RealmQueryText(body, root string) string {
	return strings.TrimPrefix(body, RealmPrefix(root))
}

// StringLiteral writes value as a double-quoted YQL string literal.
func StringLiteral(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// operators are the YQL operators longer than one character, longest first.
// `>>` is not one: the grammar reads a shift right as two `>` tokens, so a
// nested type such as `List<List<Int32>>` closes without a lexer exception.
var operators = []string{"|<<", "<<", "<=", "<>", "<|", ">=", "==", "!=", "||", "|>", "->", "::"}

// tokens splits text into the server's tokens, without whitespace and
// comments.
func tokens(text string) []string {
	var result []string
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		rest := text[i:]
		switch {
		case unicode.IsSpace(r):
			i += size
		case strings.HasPrefix(rest, "--"):
			end := strings.IndexAny(rest, "\r\n")
			if end < 0 {
				return result
			}
			i += end
		case strings.HasPrefix(rest, "/*"):
			end := strings.Index(rest[2:], "*/")
			if end < 0 {
				return result
			}
			i += 2 + end + 2
		default:
			n := tokenLength(rest)
			result = append(result, rest[:n])
			i += n
		}
	}
	return result
}

// tokenLength is the length of the token rest starts with: a quoted string or
// name, a number, a word, an operator, or one character. An unterminated
// quote takes the rest of the text, as it is one token the server refuses.
func tokenLength(rest string) int {
	r, size := utf8.DecodeRuneInString(rest)
	switch {
	case strings.HasPrefix(rest, "@@"):
		return suffixed(rest, multiline(rest))
	case r == '\'' || r == '"':
		return suffixed(rest, quoted(rest, byte(r)))
	case r == '`':
		return quoted(rest, '`')
	case r >= '0' && r <= '9':
		return number(rest)
	case word(r):
		n := size
		for n < len(rest) {
			next, width := utf8.DecodeRuneInString(rest[n:])
			if !word(next) {
				break
			}
			n += width
		}
		return n
	}
	for _, operator := range operators {
		if strings.HasPrefix(rest, operator) {
			return len(operator)
		}
	}
	return size
}

// word reports a rune of a name or a keyword.
func word(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// quoted is the length of the quoted string or name rest starts with: a
// backslash escapes the next character, and in a backticked name a doubled
// backtick stands for one.
func quoted(rest string, quote byte) int {
	for i := 1; i < len(rest); i++ {
		switch {
		case rest[i] == '\\':
			i++
		case rest[i] == quote && quote == '`' && i+1 < len(rest) && rest[i+1] == '`':
			i++
		case rest[i] == quote:
			return i + 1
		}
	}
	return len(rest)
}

// multiline is the length of the @@ string rest starts with, in which @@@@
// stands for @@.
func multiline(rest string) int {
	for i := 2; i+1 < len(rest); i++ {
		if !strings.HasPrefix(rest[i:], "@@") {
			continue
		}
		if strings.HasPrefix(rest[i:], "@@@@") {
			i += 3
			continue
		}
		return i + 2
	}
	return len(rest)
}

// suffixed extends a string of length n with the letters of its type suffix,
// such as the u of 'text'u.
func suffixed(rest string, n int) int {
	for n < len(rest) && (rest[n] >= 'a' && rest[n] <= 'z' || rest[n] >= 'A' && rest[n] <= 'Z') {
		n++
	}
	return n
}

// number is the length of the number rest starts with: its digits and
// suffix, a fraction, and an exponent with its sign, as in 2.5e-3 or 3ut.
func number(rest string) int {
	n := alnum(rest, 0)
	if n+1 < len(rest) && rest[n] == '.' && rest[n+1] >= '0' && rest[n+1] <= '9' {
		n = alnum(rest, n+1)
	}
	if last := rest[n-1]; (last == 'e' || last == 'E') && !hexadecimal(rest) && n+1 < len(rest) &&
		(rest[n] == '+' || rest[n] == '-') && rest[n+1] >= '0' && rest[n+1] <= '9' {
		n = alnum(rest, n+1)
	}
	return n
}

// hexadecimal reports a number written in base 16, whose e is a digit.
func hexadecimal(rest string) bool {
	return len(rest) > 1 && rest[0] == '0' && (rest[1] == 'x' || rest[1] == 'X')
}

// alnum skips the ASCII letters, digits and underscores of rest from i.
func alnum(rest string, i int) int {
	for i < len(rest) {
		c := rest[i]
		if c != '_' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			break
		}
		i++
	}
	return i
}
