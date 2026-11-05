package yqlquery_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
)

// The corpus holds YQL texts and the queries the host's internal/yqlquery
// cut each into, recorded by internal/rowdata/testdata/oracle at one host
// commit. A seed runs only data queries, so the split decides which seed files
// are refused before anything runs.

type query struct {
	Kind       string   `json:"kind"`
	Text       string   `json:"text"`
	Statements []string `json:"statements"`
	Source     string   `json:"source"`
	Mixed      bool     `json:"mixed"`
}

type splitRow struct {
	name    string
	text    string
	error   string
	queries []query
}

// splitTexts lists the texts the host split, in name order.
func splitTexts(c *qt.C) []splitRow {
	c.Helper()
	return slices.DeleteFunc(splitCorpus(c), func(row splitRow) bool { return row.error != "" })
}

// refusedTexts lists the texts the host refused, in name order.
func refusedTexts(c *qt.C) []splitRow {
	c.Helper()
	return slices.DeleteFunc(splitCorpus(c), func(row splitRow) bool { return row.error == "" })
}

// splitCorpus lists every text of the corpus in name order.
func splitCorpus(c *qt.C) []splitRow {
	c.Helper()
	paths, err := filepath.Glob("testdata/corpus/*.golden.json")
	c.Assert(err, qt.IsNil)
	slices.Sort(paths)
	var rows []splitRow
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".golden.json")
		data, err := os.ReadFile(path)
		c.Assert(err, qt.IsNil)
		var golden struct {
			Error   string  `json:"error"`
			Queries []query `json:"queries"`
		}
		c.Assert(json.Unmarshal(data, &golden), qt.IsNil)
		text, err := os.ReadFile(filepath.Join("testdata/corpus", name+".yql"))
		c.Assert(err, qt.IsNil)
		rows = append(rows, splitRow{name: name, text: string(text), error: golden.Error, queries: golden.Queries})
	}
	return rows
}

func TestSplitMatchesTheHost_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	rows := splitTexts(c)
	c.Assert(len(rows) >= 21, qt.IsTrue, qt.Commentf("%d split texts", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)

			queries, err := yqlquery.Split(row.text)

			c.Assert(err, qt.IsNil)
			got := make([]query, len(queries))
			for i, q := range queries {
				got[i] = query{Kind: q.Kind.String(), Text: q.Text, Statements: q.Statements, Source: q.Source, Mixed: q.Mixed}
			}
			c.Assert(got, qt.DeepEquals, row.queries)
		})
	}
}

// TestSplitSpansNameEachStatement pins that each query's spans say where its
// own statements are written, one span for each statement in the same order:
// a span starts at the statement's first token and holds the statement
// through its terminator.
func TestSplitSpansNameEachStatement(t *testing.T) {
	c := testcheck.New(t)
	for _, row := range splitTexts(c) {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)

			queries, err := yqlquery.Split(row.text)

			c.Assert(err, qt.IsNil)
			for _, q := range queries {
				c.Assert(q.Spans, qt.HasLen, len(q.Statements))
				for i, span := range q.Spans {
					written := row.text[span.Start:span.End]
					c.Assert(strings.HasPrefix(written, strings.Fields(q.Statements[i])[0]), qt.IsTrue, qt.Commentf("%q does not start %q", written, q.Statements[i]))
					c.Assert(q.Source, qt.Contains, written)
				}
			}
		})
	}
}

func TestSplitMatchesTheHost_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	rows := refusedTexts(c)
	c.Assert(len(rows) >= 3, qt.IsTrue, qt.Commentf("%d refused texts", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)

			queries, err := yqlquery.Split(row.text)

			c.Assert(err, qt.ErrorAs, new(*yqlquery.Refusal))
			c.Assert(queries, qt.IsNil)
		})
	}
}
