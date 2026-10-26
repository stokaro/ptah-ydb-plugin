package yqlschema

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"strconv"
	"strings"

	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// errUnsupported is the class of a construct the host's reader reads and this
// provider has no home for.
var errUnsupported = errors.New("unsupported")

// unsupportedError is a refusal of a construct this provider has no home for.
// Its message is the whole message.
func unsupportedError(message string) error {
	return unsupportedRefusal{message: message}
}

type unsupportedRefusal struct{ message string }

func (e unsupportedRefusal) Error() string { return e.message }

func (e unsupportedRefusal) Unwrap() error { return errUnsupported }

// errUnmodeled is the class of every statement the reader parsed and has no
// place for, spelled as the host spells it.
var errUnmodeled = errors.New("the schema model has no place for this statement")

// Document is one document of a load.
type Document struct {
	// Path names the document in a refusal.
	Path string
	// Text is the document's text.
	Text string
}

// Refusal is why a load is not read: the document, the bytes of it that are
// refused and a message naming what is refused. A refusal the host's reader
// makes with a position carries that position; one it makes about a whole
// statement carries the statement.
type Refusal struct {
	// Document is the refused document's place in the load.
	Document int
	// Start and End are byte offsets into the document's text.
	Start, End int
	// Code says what kind of refusal it is.
	Code RefusalCode
	// Message is the refusal as the host's reader words it.
	Message string
	// limit is the header entry a refusal of the load's coverage refuses.
	limit *protocol.SchemaCoverageLimit
}

// RefusalCode classifies a refusal.
type RefusalCode int

// The kinds of refusal.
const (
	// Syntax is text the grammar does not read.
	Syntax RefusalCode = iota + 1
	// Invalid is a statement the grammar reads and the model refuses.
	Invalid
	// Unsupported is a statement or a declared value the host's reader reads
	// and this provider has no home for in a SQL schema reading.
	Unsupported
)

// Read reads the documents of one load in order, as one script: each
// document is read against what the earlier ones declared, and a later
// document can change what an earlier one declared. databasePath is the
// database root absolute paths resolve against, or empty where the load names
// none. The first refusal stops the read and no reading is returned.
func Read(documents []Document, databasePath string) (*Reading, *Refusal) {
	merged := &Reading{}
	for index, document := range documents {
		statements, err := parse(document.Text)
		if err != nil {
			return nil, &Refusal{Document: index, Start: err.offset, End: syntaxEnd(document.Text, err.offset), Code: Syntax,
				Message: "YQL schema: " + err.reason}
		}
		var base *Reading
		if index > 0 {
			base = merged
		}
		c := &converter{file: &Reading{}, base: base, databasePath: databasePath, document: index}
		for _, statement := range statements {
			if err := c.convert(statement); err != nil {
				text := statement.at()
				code := Invalid
				if errors.Is(err, errUnsupported) {
					code = Unsupported
				}
				return nil, &Refusal{Document: index, Start: text.start, End: text.end, Code: code, Message: err.Error()}
			}
		}
		finalize(c.file)
		c.file.Files = []FileDeclarations{declarationsOf(c.file, statements)}
		if len(documents) == 1 {
			return c.file, nil
		}
		if repeated := merged.append(c.file); repeated != nil {
			return nil, &Refusal{Document: index, Start: repeated.span.start, End: repeated.span.end, Code: Invalid, Message: duplicateObject(*repeated).Error()}
		}
	}
	return merged, nil
}

// syntaxEnd is where the token a syntax refusal names ends.
func syntaxEnd(text string, offset int) int {
	for _, token := range yql.Tokens(text) {
		if token.Start == offset && token.Type != yql.TokenWhitespace {
			return token.End
		}
	}
	return offset
}

// converter converts the statements of one document.
type converter struct {
	file         *Reading
	base         *Reading
	databasePath string
	document     int
	statement    span
}

// readings are where a statement may find what it names: its own document
// first, then the earlier ones.
func (c *converter) readings() []*Reading {
	if c.base == nil {
		return []*Reading{c.file}
	}
	return []*Reading{c.file, c.base}
}

func (c *converter) convert(statement statement) error {
	c.statement = statement.at()
	switch node := statement.(type) {
	case *createTable:
		return c.createTable(node)
	case *addChangefeed:
		return c.addChangefeed(node)
	case *createView:
		c.file.Views = append(c.file.Views, c.view(node))
		return nil
	case *commentOn:
		return c.comment(node.text)
	case *alterSequence:
		return c.alterSequence(node)
	case *createRole:
		return c.createRole(node)
	case *alterUser:
		return c.alterUser(node)
	case *membership:
		return c.membership(node)
	case *privilege:
		return c.privilege(node)
	case *createTopic:
		return c.declareTopic(node)
	case *addConsumer:
		return c.addConsumer(node)
	case *createCoordinationNode:
		return c.declareCoordinationNode(node)
	case *createExternalSource:
		return c.declareExternalSource(node)
	case *createExternalTable:
		return c.declareExternalTable(node)
	case *createStreamingQuery:
		return c.declareStreaming(node)
	case *createPool:
		return c.declare(Object{Kind: KindResourcePool, Name: node.name, Spec: new(node.spec)})
	case *createClassifier:
		return c.declare(Object{Kind: KindResourcePoolClassifier, Name: node.name, Spec: new(node.spec)})
	case *unsupported:
		return unsupportedError(node.construct + " declares an object this provider has no model for in a SQL schema reading")
	default:
		return fmt.Errorf("%w: %T", errUnmodeled, statement)
	}
}

// identifierPart reads one written name the way YDB resolves it: a
// backticked name decoded, any other as written.
func identifierPart(written string) string {
	written = strings.TrimSpace(written)
	if name, ok := yql.IdentifierValue(written); ok {
		return name
	}
	if isQuotedPart(written) {
		return unquotePart(written)
	}
	return written
}

// splitTableName reads a written table name into its directory and its
// name: the decoded path, split at its last slash.
func splitTableName(written string) (schema, name string) {
	decoded := identifierPart(written)
	if slash := strings.LastIndex(decoded, "/"); slash >= 0 {
		return decoded[:slash], decoded[slash+1:]
	}
	return "", decoded
}

// qualifiedTableName is the qualified name a written table name reaches.
func qualifiedTableName(written string) string {
	return canonicalReference(splitTableName(written))
}

// quoteName writes a name as a YQL identifier.
func quoteName(name string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(name) + "`"
}

func isQuotedPart(value string) bool {
	if len(value) < 2 {
		return false
	}
	closeQuote := value[0]
	switch value[0] {
	case '"', '`':
	case '[':
		closeQuote = ']'
	default:
		return false
	}
	if value[len(value)-1] != closeQuote {
		return false
	}
	for index := 1; index < len(value)-1; index++ {
		if value[index] != closeQuote {
			continue
		}
		if index+1 >= len(value)-1 || value[index+1] != closeQuote {
			return false
		}
		index++
	}
	return true
}

func unquotePart(value string) string {
	switch {
	case value[0] == '"' && value[len(value)-1] == '"':
		return strings.ReplaceAll(value[1:len(value)-1], `""`, `"`)
	case value[0] == '`' && value[len(value)-1] == '`':
		return strings.ReplaceAll(value[1:len(value)-1], "``", "`")
	case value[0] == '[' && value[len(value)-1] == ']':
		return strings.ReplaceAll(value[1:len(value)-1], "]]", "]")
	default:
		return value
	}
}

func identifierParts(values []string) []string {
	if values == nil {
		return nil
	}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = identifierPart(value)
	}
	return result
}

func (c *converter) createTable(node *createTable) error {
	schema, name := splitTableName(node.name)
	table := Table{Name: name, Schema: schema, Facets: node.facets, span: c.statement, document: c.document}
	if len(node.primaryKey) > 0 {
		table.PrimaryKey = identifierParts(node.primaryKey)
		for _, column := range table.PrimaryKey {
			table.PrimaryKeyParts = append(table.PrimaryKeyParts, PrimaryKeyPart{Name: column})
		}
	}
	qualified := table.Qualified()
	c.file.Tables = append(c.file.Tables, table)
	// A single-column key is the column's own, as the host's reader marks
	// it: on the first of the statement's columns that it names.
	marked := len(table.PrimaryKey) != 1
	for _, column := range node.columns {
		field := Field{Table: qualified, Name: identifierPart(column.name), Type: column.typ, Nullable: column.nullable}
		if column.defaultValue != nil {
			field.Default, field.DefaultSet = *column.defaultValue, true
		}
		if !marked && field.Name == table.PrimaryKey[0] {
			field.Primary, marked = true, true
		}
		c.file.Fields = append(c.file.Fields, field)
	}
	for _, declared := range node.indexes {
		indexSchema, indexTable := splitTableName(declared.table)
		index := Index{
			Name: identifierPart(declared.name), Fields: identifierParts(declared.columns), Unique: declared.unique, Type: declared.kind,
			IncludeColumns: identifierParts(declared.include), StorageParams: maps.Clone(declared.storage),
			TableName: canonicalReference(indexSchema, indexTable), Partitioning: declared.partitioning, Vector: declared.vector,
			span: c.statement, document: c.document,
		}
		c.file.Indexes = append(c.file.Indexes, index)
	}
	return nil
}

// tableNamed finds the table a written name reaches, in the statement's
// document or an earlier one.
func (c *converter) tableNamed(written string) (*Reading, *Table) {
	qualified := qualifiedTableName(written)
	for _, reading := range c.readings() {
		for i := range reading.Tables {
			if reading.Tables[i].Qualified() == qualified {
				return reading, &reading.Tables[i]
			}
		}
	}
	return nil, nil
}

func (c *converter) addChangefeed(node *addChangefeed) error {
	_, table := c.tableNamed(node.table)
	if table == nil {
		return undeclaredChangefeed(node.table, node.feed.Name)
	}
	object := Object{Kind: KindChangefeed, Schema: table.Schema, Parent: table.Name, Name: node.feed.Name, Spec: new(node.feed), span: c.statement, document: c.document}
	for _, reading := range c.readings() {
		if reading.object(object) != nil {
			return fmt.Errorf("changefeed %q is declared twice on table %s", node.feed.Name, table.Qualified())
		}
	}
	c.file.Objects = append(c.file.Objects, object)
	return nil
}

func undeclaredChangefeed(written, name string) error {
	return fmt.Errorf("%w: ALTER TABLE %s ADD CHANGEFEED %s names a table this schema does not declare", errUnmodeled, written, name)
}

// object returns the object of the kind, directory, parent and name of
// wanted that reading declares, or nil.
func (r *Reading) object(wanted Object) *Object {
	for i := range r.Objects {
		held := &r.Objects[i]
		if held.Kind == wanted.Kind && held.Schema == wanted.Schema && held.Parent == wanted.Parent && held.Name == wanted.Name {
			return held
		}
	}
	return nil
}

func (c *converter) view(node *createView) View {
	schema, name := splitTableName(node.name)
	return View{Name: canonicalReference(schema, name), Body: strings.TrimSpace(node.body), schema: schema, leaf: name, span: c.statement, document: c.document}
}

// comment attaches a COMMENT ON statement to the table, column, index or
// view it names, in the statement's document or an earlier one.
func (c *converter) comment(text string) error {
	query, recognized, err := ydbcomment.Recognize(text)
	if err != nil {
		return err
	}
	if !recognized {
		return fmt.Errorf("%w: %s", errUnmodeled, text)
	}
	target := c.commentTarget(query.Statement)
	if target == nil {
		return fmt.Errorf("%w: COMMENT ON %s %s names an object this schema does not declare", errUnmodeled, query.Object.Keyword(), query.Path)
	}
	*target = query.Comment
	return nil
}

func (c *converter) commentTarget(statement ydbcomment.Statement) *string {
	written := quoteName(statement.Path)
	if statement.Object == ydbcomment.View {
		qualified := qualifiedTableName(written)
		for _, reading := range c.readings() {
			for i := range reading.Views {
				if reading.Views[i].Name == qualified {
					return &reading.Views[i].Comment
				}
			}
		}
		return nil
	}
	_, table := c.tableNamed(written)
	if table == nil {
		return nil
	}
	switch statement.Object {
	case ydbcomment.Table:
		return &table.Comment
	case ydbcomment.Column:
		name := identifierPart(quoteName(statement.Name))
		for _, reading := range c.readings() {
			for i := range reading.Fields {
				if reading.Fields[i].Table == table.Qualified() && reading.Fields[i].Name == name {
					return &reading.Fields[i].Comment
				}
			}
		}
	case ydbcomment.Index:
		for _, reading := range c.readings() {
			for i := range reading.Indexes {
				if reading.Indexes[i].TableName == table.Qualified() && reading.Indexes[i].Name == statement.Name {
					return &reading.Indexes[i].Comment
				}
			}
		}
	}
	return nil
}

// alterSequence applies an ALTER SEQUENCE to the sequence of a Serial
// column the load declares. A YDB sequence is the column's, so its START and
// INCREMENT are the column's identity settings.
func (c *converter) alterSequence(node *alterSequence) error {
	if node.start == nil && node.increment == nil {
		return fmt.Errorf("%w: desired YDB sequence settings accept only START and INCREMENT", errUnmodeled)
	}
	if !strings.HasPrefix(node.name, "/") || node.name != strings.TrimSpace(node.name) {
		return fmt.Errorf("%w: a Serial sequence needs its absolute database path", errUnmodeled)
	}
	relative, err := c.relativePath(node.name)
	if err != nil {
		return err
	}
	field := c.serialField(relative)
	if field == nil {
		return fmt.Errorf("%w: ALTER SEQUENCE must name the sequence of a Serial column declared earlier in this desired schema", errUnmodeled)
	}
	start, increment := field.IdentityStart, field.IdentityIncrement
	if node.start != nil {
		start = strconv.FormatInt(*node.start, 10)
	}
	if node.increment != nil {
		increment = strconv.FormatInt(*node.increment, 10)
	}
	if err := checkSequence(start, increment); err != nil {
		return fmt.Errorf("%w: %w", errUnmodeled, err)
	}
	field.IdentityStart, field.IdentityIncrement = start, increment
	return nil
}

// serialField is the Serial column whose sequence is at relative, or nil.
func (c *converter) serialField(relative string) *Field {
	_, table := c.tableNamed(quoteName(path.Dir(relative)))
	if table == nil {
		return nil
	}
	for _, reading := range c.readings() {
		for i := range reading.Fields {
			field := &reading.Fields[i]
			if field.Table == table.Qualified() && sequenceName(field.Name) == path.Base(relative) && declaresSerial(field.Type) {
				return field
			}
		}
	}
	return nil
}

// relativePath reads a source path relative to the database root. An
// absolute path is meaningful only with an explicit database root.
func (c *converter) relativePath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty YDB permission path", errUnmodeled)
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "." || part == ".." {
			return "", fmt.Errorf("%w: YDB source paths cannot traverse directories", errUnmodeled)
		}
	}
	name = path.Clean(name)
	if !strings.HasPrefix(name, "/") {
		return name, nil
	}
	if c.databasePath == "" {
		return "", fmt.Errorf("%w: an absolute YDB source path needs a database URL to identify its database root", errUnmodeled)
	}
	root := strings.TrimRight(c.databasePath, "/")
	if name == root {
		return "", nil
	}
	relative, ok := strings.CutPrefix(name, root+"/")
	if !ok {
		return "", fmt.Errorf("%w: YDB source path %q is outside database %q", errUnmodeled, name, root)
	}
	return relative, nil
}
