package ydbcomment_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
)

// A statement Text wrote reads back as the statement it was written from,
// whatever its comment holds and on every kind of object, a dotted index name
// and an absolute view path included, so a plan file runs the change the plan
// made. Ported from the host's TestRecognize_ReadsBackWhatTextWrote.
func TestRecognize_ReadsBackWhatTextWrote(t *testing.T) {
	comments := []string{
		"",
		"plain",
		`quotes ' and " and backticks ` + "`",
		`a backslash \ and \n written out`,
		"a newline\nand a tab\tand a return\r",
		"a NUL \x00 and DEL \x7f",
		"текст и 漢字",
		strings.Repeat("x", 4096),
		"-- not a comment; COMMENT ON TABLE t IS 'x'",
	}
	objects := []ydbcomment.Statement{
		{Object: ydbcomment.Table, Path: "shop/users"},
		{Object: ydbcomment.Column, Path: "users", Name: "e-mail`x"},
		{Object: ydbcomment.Index, Path: "dir/sub/t", Name: "a.b"},
		{Object: ydbcomment.View, Path: "/local/abs/v"},
	}
	for _, object := range objects {
		for _, comment := range comments {
			t.Run(object.Path+"/"+strings.ReplaceAll(comment[:min(12, len(comment))], "\n", " "), func(t *testing.T) {
				c := testcheck.New(t)
				statement := object
				statement.Comment = comment
				text, err := statement.Text()
				c.Assert(err, qt.IsNil)
				query, recognized, err := ydbcomment.Recognize(text)
				c.Assert(err, qt.IsNil)
				c.Assert(recognized, qt.IsTrue)
				c.Assert(query, qt.DeepEquals, ydbcomment.Query{Statement: statement})
			})
		}
	}
}

// A hand-written statement reads too: lower case, plain names, double quotes
// with the Utf8 suffix, an empty string and a lower-case NULL as a removal,
// and the translation setting at the head of the query. Ported from the
// host's TestRecognize_HappyPath.
func TestRecognize_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want ydbcomment.Query
	}{
		{name: "lower case with plain names", text: "comment on table users is 'x'",
			want: ydbcomment.Query{Statement: ydbcomment.Statement{Object: ydbcomment.Table, Path: "users", Comment: "x"}}},
		{name: "double quotes with the Utf8 suffix", text: `COMMENT ON VIEW v IS "it's"u`,
			want: ydbcomment.Query{Statement: ydbcomment.Statement{Object: ydbcomment.View, Path: "v", Comment: "it's"}}},
		{name: "an empty string removes", text: "COMMENT ON COLUMN t.c IS ''",
			want: ydbcomment.Query{Statement: ydbcomment.Statement{Object: ydbcomment.Column, Path: "t", Name: "c"}}},
		{name: "NULL in lower case", text: "COMMENT ON INDEX i ON t IS null",
			want: ydbcomment.Query{Statement: ydbcomment.Statement{Object: ydbcomment.Index, Path: "t", Name: "i"}}},
		{name: "under a prefix", text: "PRAGMA TablePathPrefix('/local/.ptah/r');\nCOMMENT ON TABLE `t` IS 'x';",
			want: ydbcomment.Query{Statement: ydbcomment.Statement{Object: ydbcomment.Table, Path: "t", Comment: "x"}, PathPrefix: "/local/.ptah/r"}},
		{name: "after the translation setting", text: "--!syntax_v1\nCOMMENT ON TABLE `t` IS 'x'",
			want: ydbcomment.Query{Statement: ydbcomment.Statement{Object: ydbcomment.Table, Path: "t", Comment: "x"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := ydbcomment.Recognize(tc.text)
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsTrue)
			c.Assert(query, qt.DeepEquals, tc.want)
		})
	}
}

// COMMENT ON another kind of object is not Ptah's statement: it reaches YDB,
// whose parse error says YQL has none. Neither is text that only mentions
// one. Ported from the host's TestRecognize_NotRecognized.
func TestRecognize_NotRecognized(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{name: "a sequence", text: "COMMENT ON SEQUENCE s IS 'x'"},
		{name: "a constraint", text: "COMMENT ON CONSTRAINT k ON t IS 'x'"},
		{name: "a schema", text: "COMMENT ON SCHEMA s IS 'x'"},
		{name: "a materialized view", text: "COMMENT ON MATERIALIZED VIEW m IS 'x'"},
		{name: "a string", text: "SELECT 'COMMENT ON TABLE t IS x'"},
		{name: "a line comment", text: "-- COMMENT ON TABLE t IS 'x'\nSELECT 1"},
		{name: "a column named comment", text: "SELECT comment FROM t"},
		{name: "no comment at all", text: "CREATE TABLE t (id Int64 NOT NULL, PRIMARY KEY (id))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := ydbcomment.Recognize(tc.text)
			c.Assert(err, qt.IsNil)
			c.Assert(recognized, qt.IsFalse)
			c.Assert(query, qt.DeepEquals, ydbcomment.Query{})
		})
	}
}

// A statement Ptah cannot run as written is refused before anything reaches
// YDB, with what to write instead. Ported from the host's
// TestRecognize_FailurePath.
func TestRecognize_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		text    string
		wantErr string
	}{
		{name: "no IS", text: "COMMENT ON TABLE t 'x'",
			wantErr: `invalid comment statement: COMMENT ON TABLE: the comment follows IS, as a string or NULL, and nothing follows it`},
		{name: "something after the comment", text: "COMMENT ON TABLE t IS 'x' 'y'",
			wantErr: `invalid comment statement: COMMENT ON TABLE: the comment follows IS, as a string or NULL, and nothing follows it`},
		{name: "no table", text: "COMMENT ON TABLE",
			wantErr: `invalid comment statement: COMMENT ON TABLE: the statement names no table`},
		{name: "a named expression", text: "COMMENT ON TABLE $t IS 'x'",
			wantErr: `invalid comment statement: COMMENT ON TABLE: \$t does not name a table; write its path in backticks`},
		{name: "a column without its table", text: "COMMENT ON COLUMN c IS 'x'",
			wantErr: "invalid comment statement: COMMENT ON COLUMN: name the column as `table`.`column`"},
		{name: "an index without its table", text: "COMMENT ON INDEX i IS 'x'",
			wantErr: "invalid comment statement: COMMENT ON INDEX: name the index as `index` ON `table`"},
		{name: "a number", text: "COMMENT ON TABLE t IS 5",
			wantErr: `invalid comment statement: COMMENT ON TABLE: the comment 5 is not a string Ptah reads; .*`},
		{name: "a raw string", text: "COMMENT ON TABLE t IS @@x@@",
			wantErr: `invalid comment statement: COMMENT ON TABLE: the comment @@x@@ is not a string Ptah reads; .*`},
		{name: "too long", text: "COMMENT ON VIEW v IS '" + strings.Repeat("x", 4097) + "'",
			wantErr: `invalid comment statement: COMMENT ON VIEW: the comment is 4097 bytes long, and YDB keeps at most 4096 bytes in an attribute value`},
		{name: "a column name too long", text: "COMMENT ON COLUMN t." + strings.Repeat("c", 81) + " IS NULL",
			wantErr: `invalid comment statement: COMMENT ON COLUMN: YDB keeps the comment as the table attribute .*; a column name takes at most 80`},
		{name: "two statements", text: "COMMENT ON TABLE t IS 'a'; COMMENT ON TABLE u IS 'b'",
			wantErr: `invalid comment statement: a query runs one COMMENT ON TABLE statement, and this one holds two; put each in a query of its own`},
		{name: "not last", text: "COMMENT ON TABLE t IS 'a'; SELECT 1",
			wantErr: `invalid comment statement: COMMENT ON TABLE is followed by another statement in its query, and Ptah runs it through YDB's table service, which takes it alone`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			query, recognized, err := ydbcomment.Recognize(tc.text)
			c.Assert(err, qt.ErrorIs, ydbcomment.ErrStatement)
			c.Assert(err, qt.ErrorMatches, tc.wantErr)
			c.Assert(recognized, qt.IsTrue)
			c.Assert(query, qt.DeepEquals, ydbcomment.Query{})
		})
	}
}

// A relative TablePathPrefix is refused, as YDB refuses it. Ported from the
// host's TestQuery_Absolute_FailurePath.
func TestQueryAbsolute_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	query := ydbcomment.Query{Statement: ydbcomment.Statement{Object: ydbcomment.Table, Path: "t"}, PathPrefix: "relp"}
	got, err := query.Absolute("/local", "")
	c.Assert(err, qt.ErrorIs, ydbcomment.ErrStatement)
	c.Assert(err, qt.ErrorMatches, `invalid comment statement: path relp/t is not in database /local: the TablePathPrefix "relp" is relative, and YDB reads a prefix only as an absolute path`)
	c.Assert(got, qt.Equals, "")
}
