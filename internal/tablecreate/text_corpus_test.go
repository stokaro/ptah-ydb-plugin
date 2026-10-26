package tablecreate_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// corpusTable is one table a document of the YQL schema corpus declares that
// this provider creates: the declaration the YQL reader reads, the native
// request it compiles to, and the document it came from.
type corpusTable struct {
	document string
	text     string
	table    *protocol.DeclaredTable
	request  string
}

// corpusTables reads every single document of the YQL schema corpus the
// reader accepts and returns each table of it that compiles to a native
// creation. A document is read alone, so a table two documents of one load
// declare is read from each.
func corpusTables(c *qt.C) []corpusTable {
	c.Helper()
	documents, err := filepath.Glob(filepath.Join("..", "yqlschema", "testdata", "corpus", "*", "*.sql"))
	c.Assert(err, qt.IsNil)
	var tables []corpusTable
	for _, document := range documents {
		text, err := os.ReadFile(document)
		c.Assert(err, qt.IsNil)
		reply := yqlschema.Reply(&protocol.ReadSQLSchemaRequest{Engine: "ydb", Documents: []*protocol.SQLDocument{{Id: "d", Path: "01.sql", Text: string(text)}}})
		for _, table := range reply.GetTables() {
			owner := &protocol.Reference{Engine: "ydb", Kind: "table", Schema: table.GetSchema(), Name: table.GetName()}
			request, err := tablecreate.Compile(owner, table, nil)
			if err != nil {
				continue
			}
			written, err := tablecreate.RowTableStatement(tablecreate.TextPath(table.GetSchema(), table.GetName()), request)
			c.Assert(err, qt.IsNil, qt.Commentf("%s: table %s", document, table.GetName()))
			tables = append(tables, corpusTable{document: document, text: string(text), table: table, request: written})
		}
	}
	return tables
}

// What this provider writes for a table reads back to the declaration it was
// written from, for every table of the corpus it creates.
func TestRowTableStatementReadsBackToItsDeclaration(t *testing.T) {
	c := qt.New(t)
	tables := corpusTables(c)
	// A glob that stopped matching would pass with no table at all.
	c.Assert(len(tables) >= 80, qt.IsTrue, qt.Commentf("%d tables", len(tables)))
	for _, table := range tables {
		back := yqlschema.Reply(&protocol.ReadSQLSchemaRequest{Engine: "ydb", Documents: []*protocol.SQLDocument{{Id: "d", Path: "01.sql", Text: table.request}}})
		c.Assert(back.GetComplete(), qt.IsTrue, qt.Commentf("%s:\n%s\n%v", table.document, table.request, back.GetDiagnostics()))
		c.Assert(back.GetTables(), qt.HasLen, 1)
		c.Assert(proto.Equal(back.GetTables()[0], withoutNullDefaults(table.table)), qt.IsTrue, qt.Commentf("%s:\n%s", table.document, table.request))
	}
}

// For a table the host's embedded engine wrote, `ptah schema render --dialect
// ydb` of a fixture or a statement a host test holds in its layout, this
// provider writes the same statement byte for byte. A statement in that
// layout opens with the quoted path and a line break.
func TestRowTableStatementMatchesTheEmbeddedRendering(t *testing.T) {
	c := qt.New(t)
	var rendered int
	for _, table := range corpusTables(c) {
		opening := "CREATE TABLE " + tablecreate.QuoteIdentifier(tablecreate.TextPath(table.table.GetSchema(), table.table.GetName())) + " (\n    "
		if !strings.Contains(table.text, opening) {
			continue
		}
		rendered++
		c.Assert(table.text, qt.Contains, strings.TrimSuffix(table.request, ";"), qt.Commentf("%s", table.document))
	}
	c.Assert(rendered >= 8, qt.IsTrue, qt.Commentf("%d rendered tables", rendered))
}

// A source NULL default is an explicit absence. YDB stores no value for it,
// and its canonical CREATE therefore has no DEFAULT clause.
func withoutNullDefaults(source *protocol.DeclaredTable) *protocol.DeclaredTable {
	result := proto.CloneOf(source)
	for _, column := range result.Columns {
		if column.DefaultSet && strings.EqualFold(column.DefaultValue, "NULL") {
			column.DefaultSet = false
			column.DefaultValue = ""
		}
	}
	return result
}
