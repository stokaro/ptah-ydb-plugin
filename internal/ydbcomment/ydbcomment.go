// Package ydbcomment reads the statements Ptah keeps a YDB comment with.
//
// YQL has no COMMENT statement. Ptah keeps a comment as a user attribute of
// the table or view it belongs to, and a column's and an index's comment on
// their table under a key that names them, and its YDB connection runs
// `COMMENT ON TABLE`, `COLUMN`, `INDEX ... ON` and `VIEW` through the table
// service. [Recognize] reads those statements as the host's
// internal/ydbcomment does, for migration lint and for the YQL schema reader,
// so both read a comment with one rule.
package ydbcomment

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/servicequery"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// Object is what a comment belongs to.
type Object int

const (
	Table Object = iota + 1
	Column
	Index
	View
)

// keyword is the word COMMENT ON names the object with.
func (o Object) Keyword() string {
	switch o {
	case Table:
		return "TABLE"
	case Column:
		return "COLUMN"
	case Index:
		return "INDEX"
	case View:
		return "VIEW"
	default:
		return ""
	}
}

// noun names the object in a sentence, with its article.
func (o Object) noun() string {
	if o == Index {
		return "an index"
	}
	return "a " + strings.ToLower(o.Keyword())
}

// The attribute keys: a table's and a view's own comment, and the prefixes a
// column's and an index's name follow.
const (
	ownCommentKey    = "ptah.comment"
	columnCommentKey = "ptah.comment.column."
	indexCommentKey  = "ptah.comment.index."
)

// YDB's limits on a user attribute, measured on 25.1.4.7 and 26.2.1.14.
const (
	maxCommentKeyBytes   = 100
	maxCommentValueBytes = 4096
)

// Key is the attribute that holds the comment of object, where name is
// the column's or the index's name.
func Key(object Object, name string) string {
	switch object {
	case Column:
		return columnCommentKey + name
	case Index:
		return indexCommentKey + name
	default:
		return ownCommentKey
	}
}

// Refusal says why YDB cannot hold comment on object, or returns ""
// when it can.
func Refusal(object Object, name, comment string) string {
	key := Key(object, name)
	switch {
	case (object == Column || object == Index) && name == "":
		return fmt.Sprintf("%s comment names no %s", object.noun(), strings.ToLower(object.Keyword()))
	case len(key) > maxCommentKeyBytes:
		return fmt.Sprintf("YDB keeps the comment as the table attribute %q, %d bytes long, and an attribute key "+
			"takes at most %d bytes; %s name takes at most %d", key, len(key), maxCommentKeyBytes,
			object.noun(), maxCommentKeyBytes-len(Key(object, "")))
	case len(comment) > maxCommentValueBytes:
		return fmt.Sprintf("the comment is %d bytes long, and YDB keeps at most %d bytes in an attribute value",
			len(comment), maxCommentValueBytes)
	case !utf8.ValidString(comment):
		return "the comment is not UTF-8 text, which YDB keeps in an attribute value"
	}
	return ""
}

// Statement is one of Ptah's comment statements.
type Statement struct {
	Object Object
	// Path is the table's or the view's path as the statement names it.
	Path string
	// Name is the column's or the index's name.
	Name string
	// Comment is what the comment becomes; empty removes it.
	Comment string
}

// Key is the attribute the statement sets.
func (s Statement) Key() string {
	return Key(s.Object, s.Name)
}

// Query is a query that runs one of Ptah's comment statements.
type Query struct {
	Statement
	// PathPrefix is the path the query's `PRAGMA TablePathPrefix` sets, or
	// empty where it sets none.
	PathPrefix string
}

var ErrStatement = errors.New("invalid comment statement")

// service is the YDB service a comment statement runs through, as a
// refusal names it.
const service = "table service"

// Recognize reports whether text is a query running one of Ptah's
// comment statements, and reads it. Recognized text that does not read as the
// statement is refused.
func Recognize(text string) (Query, bool, error) {
	if !servicequery.ContainsFold(text, "comment") {
		return Query{}, false, nil
	}
	located, recognized, err := servicequery.Locate(text, opens, service)
	switch {
	case err != nil:
		return Query{}, true, fmt.Errorf("%w: %w", ErrStatement, err)
	case !recognized:
		return Query{}, false, nil
	}
	statement, err := parse(located.Tokens)
	if err != nil {
		return Query{}, true, err
	}
	return Query{Statement: statement, PathPrefix: located.PathPrefix}, true, nil
}

// opens reports whether tokens open one of the four statements.
func opens(tokens []yql.Token) bool {
	if len(tokens) < 3 || !tokens[0].IsWord("COMMENT") || !tokens[1].IsWord("ON") {
		return false
	}
	_, known := objectOf(tokens[2])
	return known
}

// objectOf reads the object keyword of a statement.
func objectOf(token yql.Token) (Object, bool) {
	for _, object := range []Object{Table, Column, Index, View} {
		if token.IsWord(object.Keyword()) {
			return object, true
		}
	}
	return 0, false
}

// parse reads one of the four statements from its tokens.
func parse(tokens []yql.Token) (Statement, error) {
	object, _ := objectOf(tokens[2])
	statement := Statement{Object: object}
	opening := servicequery.FirstWords(tokens, 3)
	refuse := func(format string, args ...any) (Statement, error) {
		return Statement{}, fmt.Errorf("%w: %s: %s", ErrStatement, opening, fmt.Sprintf(format, args...))
	}
	rest := tokens[3:]
	var ok bool
	switch object {
	case Column:
		if len(rest) < 3 || !rest[1].IsOperator(".") {
			return refuse("name the column as `table`.`column`")
		}
		if statement.Path, ok = name(rest[0]); !ok {
			return refuse("%s does not name a table; write its path in backticks", rest[0].Value)
		}
		if statement.Name, ok = name(rest[2]); !ok {
			return refuse("%s does not name a column", rest[2].Value)
		}
		rest = rest[3:]
	case Index:
		if len(rest) < 3 || !rest[1].IsWord("ON") {
			return refuse("name the index as `index` ON `table`")
		}
		if statement.Name, ok = name(rest[0]); !ok {
			return refuse("%s does not name an index", rest[0].Value)
		}
		if statement.Path, ok = name(rest[2]); !ok {
			return refuse("%s does not name a table; write its path in backticks", rest[2].Value)
		}
		rest = rest[3:]
	default:
		if len(rest) == 0 {
			return refuse("the statement names no %s", strings.ToLower(object.Keyword()))
		}
		if statement.Path, ok = name(rest[0]); !ok {
			return refuse("%s does not name %s; write its path in backticks", rest[0].Value, object.noun())
		}
		rest = rest[1:]
	}
	if len(rest) != 2 || !rest[0].IsWord("IS") {
		return refuse("the comment follows IS, as a string or NULL, and nothing follows it")
	}
	if !rest[1].IsWord("NULL") {
		comment, isString := servicequery.StringValue(rest[1])
		if !isString {
			return refuse("the comment %s is not a string Ptah reads; write it in quotes, with the escapes "+
				"\\\\, \\', \\\", \\n, \\r, \\t and \\xHH", rest[1].Value)
		}
		statement.Comment = comment
	}
	if reason := Refusal(statement.Object, statement.Name, statement.Comment); reason != "" {
		return refuse("%s", reason)
	}
	return statement, nil
}

// name reads a backticked or plain identifier. A named expression is
// not a name: the statement is read before YDB would evaluate one.
func name(token yql.Token) (string, bool) {
	if token.Type != yql.TokenIdentifier || strings.HasPrefix(token.Value, "$") {
		return "", false
	}
	value, ok := yql.IdentifierValue(token.Value)
	return value, ok && value != ""
}
