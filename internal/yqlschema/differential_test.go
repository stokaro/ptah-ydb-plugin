package yqlschema_test

import (
	"maps"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlschema"
)

// The corpus is the measurement, so it must not shrink without notice and
// every golden file must come from one run of one host commit.
func TestCorpusShape(t *testing.T) {
	c := qt.New(t)
	rows := corpus(c, func(row) bool { return true })
	c.Assert(len(rows) >= 800, qt.IsTrue, qt.Commentf("the corpus holds %d rows", len(rows)))
	hosts := map[string]bool{}
	for _, r := range rows {
		hosts[r.golden.Host] = true
		c.Assert(r.golden.Host, qt.Matches, `[0-9a-f]{40}`, qt.Commentf(r.name))
	}
	c.Assert(slices.Collect(maps.Keys(hosts)), qt.HasLen, 1)
	c.Assert(len(corpus(c, refused)) >= 500, qt.IsTrue)
	c.Assert(len(corpus(c, accepted)) >= 280, qt.IsTrue)
	names := map[string]bool{}
	for _, r := range rows {
		names[r.name] = true
	}
	for name := range unsupportedRows(c) {
		c.Assert(names[name], qt.IsTrue, qt.Commentf("%s is listed and not in the corpus", name))
	}
}

// TestDifferential_FailurePath requires the provider to refuse every load the
// host refuses, at the document and the position the host names, with the
// host's words.
func TestDifferential_FailurePath(t *testing.T) {
	listed := unsupportedRows(qt.New(t))
	rows := corpus(qt.New(t), func(r row) bool { return refused(r) && listed[r.name] == "" })
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			reading, refusal := yqlschema.Read(r.documents, r.golden.DatabasePath)
			c.Assert(reading, qt.IsNil)
			c.Assert(refusal, qt.IsNotNil, qt.Commentf("the host refuses it with: %s", r.golden.Grammar.Error))
			want := expectedRefusal(c, r.golden.Grammar.Error)
			c.Assert(pluginRefusal(refusal, want), qt.DeepEquals, want)
			c.Assert(refusal.Code, qt.Not(qt.Equals), yqlschema.Unsupported)
		})
	}
}

// TestDifferential_HappyPath reads every load the host accepts and requires
// the reading the host's grammar makes: the same tables, columns, indexes,
// views, principals, grants and YDB objects, with the same settings, and the
// same declarations for each document.
func TestDifferential_HappyPath(t *testing.T) {
	listed := unsupportedRows(qt.New(t))
	rows := corpus(qt.New(t), func(r row) bool { return accepted(r) && listed[r.name] == "" })
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			reading, refusal := yqlschema.Read(r.documents, r.golden.DatabasePath)
			c.Assert(refusal, qt.IsNil)
			c.Assert(pluginMeaning(c, reading), qt.DeepEquals, hostMeaning(c, r.golden.Grammar))
		})
	}
}

// TestDifferential_Unsupported requires the provider to refuse, by the name of
// the construct, every load testdata/unsupported.txt lists: a load holding a
// statement the host reads into an object this provider has no model for.
func TestDifferential_Unsupported(t *testing.T) {
	listed := unsupportedRows(qt.New(t))
	rows := corpus(qt.New(t), func(r row) bool { return listed[r.name] != "" })
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c := qt.New(t)
			reading, refusal := yqlschema.Read(r.documents, r.golden.DatabasePath)
			c.Assert(reading, qt.IsNil)
			c.Assert(refusal, qt.IsNotNil)
			c.Assert(refusal.Code, qt.Equals, yqlschema.Unsupported)
			c.Assert(refusal.Message, qt.Matches, `^`+listed[r.name]+` .*`)
		})
	}
}
