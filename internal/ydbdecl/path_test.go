package ydbdecl_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbdecl"
)

// An external object's path reads relative to the database root: the last
// slash separates the directory from the name, a dot stays in the name, and
// surrounding space is ignored. Ported from the host's TestSplit_HappyPath.
func TestParseExternalPath_HappyPath(t *testing.T) {
	for _, test := range []struct {
		written string
		want    ydbdecl.Path
	}{
		{written: "pg", want: ydbdecl.Path{Name: "pg"}},
		{written: "pg.pw", want: ydbdecl.Path{Name: "pg.pw"}},
		{written: "ext/pg", want: ydbdecl.Path{Schema: "ext", Name: "pg"}},
		{written: " app/ext/pg.pw ", want: ydbdecl.Path{Schema: "app/ext", Name: "pg.pw"}},
	} {
		t.Run(test.written, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbdecl.ParseExternalPath(test.written)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
		})
	}
}

// A topic's path reads relative to the database root: a slash separates
// directories, a dot stays in its segment, a changefeed's topic sits two
// directories down, and surrounding space is ignored. Ported from the host's
// TestParsePath_HappyPath.
func TestParseTopicPath_HappyPath(t *testing.T) {
	for _, test := range []struct {
		written string
		want    ydbdecl.Path
	}{
		{written: "events.v1", want: ydbdecl.Path{Name: "events.v1"}},
		{written: "app/events", want: ydbdecl.Path{Schema: "app", Name: "events"}},
		{written: "app/orders/updates", want: ydbdecl.Path{Schema: "app/orders", Name: "updates"}},
		{written: " app/events ", want: ydbdecl.Path{Schema: "app", Name: "events"}},
	} {
		t.Run(test.written, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbdecl.ParseTopicPath(test.written)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
		})
	}
}

// A topic path with no name, or with an empty or parent segment, a trailing
// slash included, is refused, and so is one written from the server root.
// Ported from the host's TestParsePath_FailurePath.
func TestParseTopicPath_FailurePath(t *testing.T) {
	for _, test := range []struct {
		written string
		want    string
	}{
		{written: "", want: `"" is not a topic path \(dir/name\): invalid feature value: a topic requires a schema-scoped YDB identity`},
		{written: "app/..", want: `"app/\.\." is not a topic path \(dir/name\): invalid feature value: a topic requires a schema-scoped YDB identity`},
		{written: "app//events", want: `"app//events" is not a topic path \(dir/name\): invalid feature value: a topic requires a schema-scoped YDB identity`},
		{written: "app/events/", want: `"app/events/" is not a topic path \(dir/name\): invalid feature value: a topic requires a schema-scoped YDB identity`},
		{written: "/local/app/events", want: `"/local/app/events" is not a topic path \(dir/name\): invalid feature value: the path starts with a slash; .*`},
	} {
		t.Run(test.written, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := ydbdecl.ParseTopicPath(test.written)
			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(got, qt.DeepEquals, ydbdecl.Path{})
		})
	}
}
