package readproof_test

import (
	"context"
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Scheme"

	"github.com/stokaro/ptah-ydb-plugin/internal/readproof"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// readCatalog is a database of a few objects by absolute path, and records
// every path it was asked the type of.
type readCatalog struct {
	types map[string]Ydb_Scheme.Entry_Type
	views map[string]string
	asked []string
}

func (c *readCatalog) Database() string { return "/local" }

func (c *readCatalog) EntryType(_ context.Context, path string) (Ydb_Scheme.Entry_Type, error) {
	c.asked = append(c.asked, path)
	entryType, found := c.types[path]
	if !found {
		return Ydb_Scheme.Entry_TYPE_UNSPECIFIED, errors.New("SCHEME_ERROR: Path not found")
	}
	return entryType, nil
}

func (c *readCatalog) ViewQuery(_ context.Context, path string) (string, error) {
	return c.views[path], nil
}

// newReadCatalog holds a row table, a column table, a system view, an
// external table, its data source, a topic, views over some of them, a view
// created under a realm's prefix, and a chain of views deeper than the proof
// follows.
func newReadCatalog() *readCatalog {
	return &readCatalog{
		types: map[string]Ydb_Scheme.Entry_Type{
			"/local/orders":                     Ydb_Scheme.Entry_TABLE,
			"/local/metrics":                    Ydb_Scheme.Entry_COLUMN_TABLE,
			"/local/.sys/partition_stats":       Ydb_Scheme.Entry_SYS_VIEW,
			"/local/ext/events":                 Ydb_Scheme.Entry_EXTERNAL_TABLE,
			"/local/ext/s3":                     Ydb_Scheme.Entry_EXTERNAL_DATA_SOURCE,
			"/local/feed":                       Ydb_Scheme.Entry_TOPIC,
			"/local/realm/items":                Ydb_Scheme.Entry_TABLE,
			"/local/views/orders":               Ydb_Scheme.Entry_VIEW,
			"/local/views/events":               Ydb_Scheme.Entry_VIEW,
			"/local/views/of_events":            Ydb_Scheme.Entry_VIEW,
			"/local/views/file":                 Ydb_Scheme.Entry_VIEW,
			"/local/views/realm_items":          Ydb_Scheme.Entry_VIEW,
			"/local/views/v1":                   Ydb_Scheme.Entry_VIEW,
			"/local/views/v2":                   Ydb_Scheme.Entry_VIEW,
			"/local/views/v3":                   Ydb_Scheme.Entry_VIEW,
			"/local/views/v4":                   Ydb_Scheme.Entry_VIEW,
			"/local/views/v5":                   Ydb_Scheme.Entry_VIEW,
			"/local/views/root_relative_orders": Ydb_Scheme.Entry_VIEW,
		},
		views: map[string]string{
			"/local/views/orders":               "SELECT * FROM orders",
			"/local/views/events":               "SELECT * FROM `ext/events`",
			"/local/views/of_events":            "SELECT * FROM `views/events`",
			"/local/views/file":                 "SELECT FileContent('secrets') FROM orders",
			"/local/views/realm_items":          "PRAGMA TablePathPrefix ( '/local/realm' ) ; SELECT * FROM items",
			"/local/views/v1":                   "SELECT * FROM `views/v2`",
			"/local/views/v2":                   "SELECT * FROM `views/v3`",
			"/local/views/v3":                   "SELECT * FROM `views/v4`",
			"/local/views/v4":                   "SELECT * FROM `views/v5`",
			"/local/views/v5":                   "SELECT * FROM orders",
			"/local/views/root_relative_orders": "SELECT * FROM orders",
		},
	}
}

// The proof asks the catalog about every object a query reads, through every
// view, and about nothing else: a name resolves against the root the query
// is sent to, an absolute one stands, and a view's names resolve against the
// database root or the prefix the view was created under. Ported from the
// host's TestProveLocalReads_HappyPath.
func TestProveLocalReads_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		root  string
		query string
		asked []string
	}{
		{name: "no source", root: "/local", query: "SELECT 1"},
		{name: "a row table", root: "/local", query: "SELECT COUNT(*) FROM orders", asked: []string{"/local/orders"}},
		{name: "a column table and a system view", root: "/local",
			query: "SELECT * FROM metrics AS m JOIN `.sys/partition_stats` AS p ON m.id = p.id",
			asked: []string{"/local/metrics", "/local/.sys/partition_stats"}},
		{name: "an absolute path", root: "/local/realm", query: "SELECT * FROM `/local/orders`", asked: []string{"/local/orders"}},
		{name: "a relative name in a realm", root: "/local/realm", query: "SELECT * FROM items", asked: []string{"/local/realm/items"}},
		{name: "a view over a table", root: "/local", query: "SELECT * FROM `views/orders`",
			asked: []string{"/local/views/orders", "/local/orders"}},
		{name: "a view created under a realm's prefix", root: "/local", query: "SELECT * FROM `views/realm_items`",
			asked: []string{"/local/views/realm_items", "/local/realm/items"}},
		{name: "a view read from a realm resolves its names at the database root", root: "/local/realm",
			query: "SELECT * FROM `/local/views/root_relative_orders`",
			asked: []string{"/local/views/root_relative_orders", "/local/orders"}},
		{name: "views as deep as the proof follows", root: "/local", query: "SELECT * FROM `views/v2`",
			asked: []string{"/local/views/v2", "/local/views/v3", "/local/views/v4", "/local/views/v5", "/local/orders"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			catalog := newReadCatalog()
			err := readproof.ProveLocalReads(c.Context(), catalog, tc.root, tc.query)
			c.Assert(err, qt.IsNil)
			c.Assert(catalog.asked, qt.DeepEquals, tc.asked)
		})
	}
}

// A read of an external table, wherever the query or a view puts it, is
// refused with ErrReadLeavesDatabase. Ported from the host's
// TestProveLocalReads_RefusesAReadThatLeavesTheDatabase.
func TestProveLocalReads_RefusesAReadThatLeavesTheDatabase(t *testing.T) {
	for _, tc := range []struct{ name, query, want string }{
		{name: "an external table", query: "SELECT COUNT(*) FROM `ext/events`",
			want: `the query reads rows from outside the database: /local/ext/events is an external table, ` +
				`whose rows the server fetches from its data source`},
		{name: "an external table joined to a table", query: "SELECT * FROM orders AS o JOIN `ext/events` AS e ON o.id = e.id",
			want: `the query reads rows from outside the database: /local/ext/events is an external table, .*`},
		{name: "an external table in a subquery", query: "SELECT 1 FROM orders WHERE id IN (SELECT id FROM `ext/events`)",
			want: `the query reads rows from outside the database: /local/ext/events is an external table, .*`},
		{name: "a view over an external table", query: "SELECT * FROM `views/events`",
			want: `the query reads the view /local/views/events: the query reads rows from outside the database: ` +
				`/local/ext/events is an external table, .*`},
		{name: "a view over that view", query: "SELECT * FROM `views/of_events`",
			want: `the query reads the view /local/views/of_events: the query reads the view /local/views/events: .*`},
		{name: "a path in another database", query: "SELECT * FROM `/other/orders`",
			want: `the query reads rows from outside the database: /other/orders is outside the session database`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			err := readproof.ProveLocalReads(c.Context(), newReadCatalog(), "/local", tc.query)
			c.Assert(err, qt.ErrorIs, readproof.ErrReadLeavesDatabase)
			c.Assert(err, qt.ErrorMatches, tc.want)
		})
	}
}

// A read the proof cannot follow is refused too, and is not reported as one
// that leaves the database: an object it cannot describe, a kind of object it
// does not follow, views deeper than it follows, and a source the text cannot
// name. Ported from the host's TestProveLocalReads_RefusesAReadItCannotFollow.
func TestProveLocalReads_RefusesAReadItCannotFollow(t *testing.T) {
	for _, tc := range []struct{ name, query, want string }{
		{name: "a missing object", query: "SELECT * FROM nosuch",
			want: `the query reads /local/nosuch, which cannot be described: SCHEME_ERROR: Path not found`},
		{name: "a topic", query: "SELECT * FROM feed", want: `the query reads /local/feed, a TOPIC, which the proof does not follow`},
		{name: "a data source", query: "SELECT * FROM `ext/s3`",
			want: `the query reads /local/ext/s3, a EXTERNAL_DATA_SOURCE, which the proof does not follow`},
		{name: "views deeper than the proof follows", query: "SELECT * FROM `views/v1`",
			want: `.*the query reads the view /local/views/v5 through more than 4 views, which the proof does not follow`},
		{name: "a UDF, whose module name reads as a cluster", query: "SELECT Python::f(@@def f(): pass@@)() FROM orders",
			want: `the objects a YDB query reads cannot be named from its text: .*a colon inside it may name a cluster`},
		{name: "a source the text cannot name", query: "SELECT * FROM AS_TABLE($rows)",
			want: `the objects a YDB query reads cannot be named from its text: AS_TABLE is called at a source position`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			err := readproof.ProveLocalReads(c.Context(), newReadCatalog(), "/local", tc.query)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(err, qt.Not(qt.ErrorIs), readproof.ErrReadLeavesDatabase)
		})
	}
}

// A file or secret function is refused in the query and in a view it reads,
// as the host's proof refused it; the refusal names the function. Ported from
// the host's TestProveLocalReads_RefusesAReadThatLeavesTheDatabase, whose
// file rows the provider words as a call it cannot prove local.
func TestProveLocalReads_RefusesAFileRead(t *testing.T) {
	for _, tc := range []struct{ name, query, want string }{
		{name: "a file read in the query itself", query: "SELECT FileContent('secrets') FROM orders",
			want: `the query calls FILECONTENT, whose effects cannot be proved local to the database`},
		{name: "a view reading a file", query: "SELECT * FROM `views/file`",
			want: `the query reads the view /local/views/file: the query calls FILECONTENT, whose effects cannot be proved local to the database`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			err := readproof.ProveLocalReads(c.Context(), newReadCatalog(), "/local", tc.query)
			c.Assert(err, qt.ErrorMatches, tc.want)
		})
	}
}
