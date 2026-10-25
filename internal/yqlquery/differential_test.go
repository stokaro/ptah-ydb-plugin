package yqlquery_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

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
	Mixed      bool     `json:"mixed"`
}

type splitRow struct {
	name    string
	text    string
	error   string
	queries []query
}

// splitCorpus lists the texts the host split, or the ones it refused, in name
// order.
func splitCorpus(c *qt.C, split bool) []splitRow {
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
		if (golden.Error == "") == split {
			rows = append(rows, splitRow{name: name, text: string(text), error: golden.Error, queries: golden.Queries})
		}
	}
	return rows
}

func TestSplitMatchesTheHost_HappyPath(t *testing.T) {
	c := qt.New(t)
	rows := splitCorpus(c, true)
	c.Assert(len(rows) >= 18, qt.IsTrue, qt.Commentf("%d split texts", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)

			queries, err := yqlquery.Split(row.text)

			c.Assert(err, qt.IsNil)
			got := make([]query, len(queries))
			for i, q := range queries {
				got[i] = query{Kind: q.Kind.String(), Text: q.Text, Statements: q.Statements, Mixed: q.Mixed}
			}
			c.Assert(got, qt.DeepEquals, row.queries)
		})
	}
}

func TestSplitMatchesTheHost_FailurePath(t *testing.T) {
	c := qt.New(t)
	rows := splitCorpus(c, false)
	c.Assert(len(rows) >= 3, qt.IsTrue, qt.Commentf("%d refused texts", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := qt.New(t)

			queries, err := yqlquery.Split(row.text)

			c.Assert(err, qt.ErrorAs, new(*yqlquery.Refusal))
			c.Assert(queries, qt.IsNil)
		})
	}
}
