// Package ydbcomment stores Ptah's comments on YDB objects, as the host's
// internal/ydbcomment stores them.
//
// YQL has no COMMENT statement and no comment clause. What YDB has is a user
// attribute, a key and a value on a scheme object, which the table service
// sets and DescribeTable reports. A row table and a view hold attributes; an
// index, a directory and the database root do not. So a comment is an
// attribute of the table or view it belongs to, and a column's and an index's
// comment live on their table, under a key that names them; see [Key].
//
// The package names the keys, holds YDB's limits on them, reads comments back
// out of an object's attributes, and writes and reads the statement Ptah runs
// through the table service: `COMMENT ON TABLE`, `COLUMN`, `INDEX ... ON` and
// `VIEW`, in the PostgreSQL family's grammar. The statement is Ptah's own;
// another client sending it to YDB gets a parse error. Migration lint, the
// YQL schema reader, the renderer and execution read it with one rule.
package ydbcomment

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-ydb-plugin/internal/servicequery"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbtype"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

// Object is what a comment belongs to.
type Object int

// The objects a YDB comment can belong to.
const (
	// Table is a row table's own comment.
	Table Object = iota + 1
	// Column is a column's comment, kept on its table.
	Column
	// Index is an index's comment, kept on its table: an index path holds no
	// attribute.
	Index
	// View is a view's own comment.
	View
)

// Keyword is the word COMMENT ON names the object with, and empty for a value
// that names no object.
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

// The attribute keys. A table's and a view's own comment is [OwnKey]; a
// column's is [ColumnKeyPrefix] followed by the column's name, and an index's
// [IndexKeyPrefix] followed by the index's name, both on the table. YDB keeps
// every key whole, so the name follows the prefix as it is.
const (
	OwnKey          = "ptah.comment"
	ColumnKeyPrefix = "ptah.comment.column."
	IndexKeyPrefix  = "ptah.comment.index."
)

// YDB's limits on user attributes, measured by the host alike on 25.1.4.7
// and 26.2.1.14.
const (
	// MaxKeyBytes is the longest key: a longer one answers `alter_attributes's
	// key length is not in [1; 100]`.
	MaxKeyBytes = 100
	// MaxValueBytes is the longest value, counted in bytes.
	MaxValueBytes = 4096
	// MaxObjectBytes is what an object's attributes may take together, each
	// key's bytes and its value's. The attributes Ptah does not own count too.
	MaxObjectBytes = 10240
)

// Key is the attribute that holds the comment of object, where name is the
// column's or the index's name; a table's and a view's own key takes none.
func Key(object Object, name string) string {
	switch object {
	case Column:
		return ColumnKeyPrefix + name
	case Index:
		return IndexKeyPrefix + name
	default:
		return OwnKey
	}
}

// IsKey reports an attribute key that holds one of Ptah's comments: one a
// comment statement sets and [Read] reads. Such an attribute belongs to the
// comment, never to a table's or a view's own declared attributes.
func IsKey(key string) bool {
	return key == OwnKey || strings.HasPrefix(key, ColumnKeyPrefix) || strings.HasPrefix(key, IndexKeyPrefix)
}

// Comments are the comments an object's attributes hold under Ptah's keys.
type Comments struct {
	// Own is the table's or the view's own comment.
	Own string
	// Columns are the columns' comments, by column name.
	Columns map[string]string
	// Indexes are the indexes' comments, by index name.
	Indexes map[string]string
}

// Read returns the comments attributes hold. An attribute under no key of
// Ptah's is not a comment and is left out, and so is one with an empty value,
// which YDB never stores. The maps are nil where no such comment exists.
func Read(attributes map[string]string) Comments {
	var comments Comments
	for _, key := range slices.Sorted(maps.Keys(attributes)) {
		value := attributes[key]
		if value == "" {
			continue
		}
		if key == OwnKey {
			comments.Own = value
			continue
		}
		if name, found := strings.CutPrefix(key, ColumnKeyPrefix); found && name != "" {
			if comments.Columns == nil {
				comments.Columns = make(map[string]string)
			}
			comments.Columns[name] = value
			continue
		}
		if name, found := strings.CutPrefix(key, IndexKeyPrefix); found && name != "" {
			if comments.Indexes == nil {
				comments.Indexes = make(map[string]string)
			}
			comments.Indexes[name] = value
		}
	}
	return comments
}

// Attributes are the attributes, by key, that hold the comments: the
// object's own comment, and the comments of the columns and the indexes it
// names. Each empty comment is left out.
func (c Comments) Attributes() map[string]string {
	attributes := make(map[string]string)
	if c.Own != "" {
		attributes[OwnKey] = c.Own
	}
	for name, comment := range c.Columns {
		if comment != "" {
			attributes[Key(Column, name)] = comment
		}
	}
	for name, comment := range c.Indexes {
		if comment != "" {
			attributes[Key(Index, name)] = comment
		}
	}
	return attributes
}

// SizeRefusal says why an object cannot hold the comments c, or returns ""
// when it can: together they take more than [MaxObjectBytes]. subject names
// the object.
func (c Comments) SizeRefusal(subject string) string {
	size := 0
	for key, value := range c.Attributes() {
		size += len(key) + len(value)
	}
	if size <= MaxObjectBytes {
		return ""
	}
	return fmt.Sprintf("the comments of %s take %d bytes as YDB table attributes, keys and text together, "+
		"and YDB keeps at most %d bytes of attributes on one object", subject, size, MaxObjectBytes)
}

// Refusal says why YDB cannot hold comment on object, named name for a column
// or an index, or returns "" when it can. An empty comment is no comment and
// takes nothing but its key. The reasons are the server's limits: a key
// longer than [MaxKeyBytes], a value longer than [MaxValueBytes], and a value
// that is not UTF-8, which the table service's request cannot carry.
func Refusal(object Object, name, comment string) string {
	key := Key(object, name)
	switch {
	case (object == Column || object == Index) && name == "":
		return fmt.Sprintf("%s comment names no %s", object.noun(), strings.ToLower(object.Keyword()))
	case len(key) > MaxKeyBytes:
		return fmt.Sprintf("YDB keeps the comment as the table attribute %q, %d bytes long, and an attribute key "+
			"takes at most %d bytes; %s name takes at most %d", key, len(key), MaxKeyBytes,
			object.noun(), MaxKeyBytes-len(Key(object, "")))
	case len(comment) > MaxValueBytes:
		return fmt.Sprintf("the comment is %d bytes long, and YDB keeps at most %d bytes in an attribute value",
			len(comment), MaxValueBytes)
	case !utf8.ValidString(comment):
		return "the comment is not UTF-8 text, which YDB keeps in an attribute value"
	}
	return ""
}

// Statement is one of Ptah's comment statements:
//
//	COMMENT ON TABLE `path` IS 'text'
//	COMMENT ON COLUMN `path`.`column` IS 'text'
//	COMMENT ON INDEX `index` ON `path` IS 'text'
//	COMMENT ON VIEW `path` IS 'text'
//
// `IS NULL` removes the comment.
type Statement struct {
	Object Object
	// Path is the table's or the view's path as the statement names it:
	// relative to the TablePathPrefix in effect, or to the database root
	// where none is, or absolute where it starts with a slash.
	Path string
	// Name is the column's or the index's name, and empty for a table or a
	// view.
	Name string
	// Comment is what the comment becomes; empty removes it.
	Comment string
}

// Key is the attribute the statement sets.
func (s Statement) Key() string {
	return Key(s.Object, s.Name)
}

// Text writes the statement with its terminating semicolon, as the host's
// embedded engine writes it. The comment is a single-quoted YQL string
// literal, and an empty one is written NULL. A comment YDB cannot hold is
// refused with [ErrStatement].
func (s Statement) Text() (string, error) {
	keyword := s.Object.Keyword()
	if keyword == "" {
		return "", fmt.Errorf("%w: unknown object %d", ErrStatement, s.Object)
	}
	if reason := Refusal(s.Object, s.Name, s.Comment); reason != "" {
		return "", fmt.Errorf("%w: %s", ErrStatement, reason)
	}
	path := ydbtype.QuoteIdentifier(s.Path)
	target := path
	switch s.Object {
	case Column:
		target = path + "." + ydbtype.QuoteIdentifier(s.Name)
	case Index:
		target = ydbtype.QuoteIdentifier(s.Name) + " ON " + path
	}
	value := "NULL"
	if s.Comment != "" {
		value = ydbtype.StringLiteral(s.Comment)
	}
	return "COMMENT ON " + keyword + " " + target + " IS " + value + ";", nil
}

// Query is a query that runs one of Ptah's comment statements.
type Query struct {
	Statement
	// PathPrefix is the path the query's `PRAGMA TablePathPrefix` sets, or
	// empty where it sets none.
	PathPrefix string
}

// Absolute is the absolute path of the table or view the query names, on the
// database whose absolute path is database, with prefix in effect where the
// query sets no TablePathPrefix of its own; see [servicequery.Absolute].
func (q Query) Absolute(database, prefix string) (string, error) {
	absolute, err := servicequery.Absolute(q.Path, q.PathPrefix, database, prefix)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrStatement, err)
	}
	return absolute, nil
}

// Opens reports whether tokens, a statement's significant tokens, open one of
// the four comment statements.
func Opens(tokens []yql.Token) bool { return opens(tokens) }

// ErrStatement is returned for text that is one of Ptah's comment statements
// and cannot be run as written.
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
