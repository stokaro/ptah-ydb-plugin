package ydbcomment_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
)

func TestRead(t *testing.T) {
	c := qt.New(t)
	comments := ydbcomment.Read(map[string]string{
		"ptah.comment": "rows", "ptah.comment.column.id": "the key", "ptah.comment.index.by.dot": "dotted",
		"ptah.comment.column.": "no name", "ptah.comment.column.empty": "", "purpose": "not a comment",
	})
	c.Assert(comments, qt.DeepEquals, ydbcomment.Comments{Own: "rows", Columns: map[string]string{"id": "the key"}, Indexes: map[string]string{"by.dot": "dotted"}})
	c.Assert(comments.Attributes(), qt.DeepEquals, map[string]string{"ptah.comment": "rows", "ptah.comment.column.id": "the key", "ptah.comment.index.by.dot": "dotted"})
	c.Assert(ydbcomment.Read(nil), qt.DeepEquals, ydbcomment.Comments{})
}

func TestIsKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{"ptah.comment", true}, {"ptah.comment.column.id", true}, {"ptah.comment.index.i", true},
		{"ptah.commentary", false}, {"purpose", false}, {"__async_replica", false},
	} {
		t.Run(tc.key, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(ydbcomment.IsKey(tc.key), qt.Equals, tc.want)
		})
	}
}

func TestSizeRefusal(t *testing.T) {
	c := qt.New(t)
	c.Assert(ydbcomment.Comments{Own: strings.Repeat("x", 4096), Columns: map[string]string{"a": strings.Repeat("y", 4096)}}.SizeRefusal("table t"), qt.Equals, "")
	c.Assert(ydbcomment.Comments{Own: strings.Repeat("x", 4096), Columns: map[string]string{"a": strings.Repeat("y", 4096), "b": strings.Repeat("z", 2000)}}.SizeRefusal("table t"),
		qt.Equals, "the comments of table t take 10246 bytes as YDB table attributes, keys and text together, and YDB keeps at most 10240 bytes of attributes on one object")
}

// Each statement Text writes reads back as itself.
func TestStatementText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statement ydbcomment.Statement
		want      string
	}{
		{"table", ydbcomment.Statement{Object: ydbcomment.Table, Path: "app/t", Comment: "it's"}, "COMMENT ON TABLE `app/t` IS 'it\\'s';"},
		{"column", ydbcomment.Statement{Object: ydbcomment.Column, Path: "t", Name: "c", Comment: "tab\there"}, "COMMENT ON COLUMN `t`.`c` IS 'tab\\there';"},
		{"index removal", ydbcomment.Statement{Object: ydbcomment.Index, Path: "t", Name: "by.c"}, "COMMENT ON INDEX `by.c` ON `t` IS NULL;"},
		{"view", ydbcomment.Statement{Object: ydbcomment.View, Path: "v`w", Comment: "привет"}, "COMMENT ON VIEW `v\\`w` IS 'привет';"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			text, err := tc.statement.Text()
			c.Assert(err, qt.IsNil)
			c.Assert(text, qt.Equals, tc.want)
			query, recognized, err := ydbcomment.Recognize(text)
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsTrue)
			c.Assert(query.Statement, qt.Equals, tc.statement)
		})
	}
}

func TestStatementText_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statement ydbcomment.Statement
		want      string
	}{
		{"no object", ydbcomment.Statement{Path: "t"}, `invalid comment statement: unknown object 0`},
		{"column without a name", ydbcomment.Statement{Object: ydbcomment.Column, Path: "t", Comment: "x"}, `invalid comment statement: a column comment names no column`},
		{"value too long", ydbcomment.Statement{Object: ydbcomment.Table, Path: "t", Comment: strings.Repeat("x", 4097)},
			`invalid comment statement: the comment is 4097 bytes long, and YDB keeps at most 4096 bytes in an attribute value`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			text, err := tc.statement.Text()
			c.Assert(err, qt.ErrorIs, ydbcomment.ErrStatement)
			c.Assert(err, qt.ErrorMatches, tc.want)
			c.Assert(text, qt.Equals, "")
		})
	}
}

func TestQueryAbsolute(t *testing.T) {
	for _, tc := range []struct {
		name, text, prefix, want string
	}{
		{"relative to the database", "COMMENT ON TABLE `app/t` IS 'x'", "", "/local/app/t"},
		{"relative to the session's realm", "COMMENT ON TABLE `t` IS 'x'", "/local/ptah_dev/r1", "/local/ptah_dev/r1/t"},
		{"under the query's prefix", "PRAGMA TablePathPrefix(\"/local/app\");\nCOMMENT ON TABLE `t` IS 'x'", "/local/ptah_dev/r1", "/local/app/t"},
		{"absolute", "COMMENT ON TABLE `/local/x/t` IS 'x'", "", "/local/x/t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			query, recognized, err := ydbcomment.Recognize(tc.text)
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsTrue)
			absolute, err := query.Absolute("/local", tc.prefix)
			c.Assert(err, qt.IsNil)
			c.Assert(absolute, qt.Equals, tc.want)
		})
	}
}
