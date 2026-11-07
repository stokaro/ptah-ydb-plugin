package ydbcomment_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbcomment"
)

// A comment YDB can hold is accepted up to each limit, which is counted in
// bytes: a key of 100 bytes, which leaves a column name 80 and an index name
// 81, and a value of 4096 bytes. Ported from the host's TestRefusal_HappyPath.
func TestRefusal_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		object  ydbcomment.Object
		of      string
		comment string
	}{
		{name: "table", object: ydbcomment.Table, comment: "x"},
		{name: "no comment", object: ydbcomment.Column, of: "c"},
		{name: "a column name of 80 bytes", object: ydbcomment.Column, of: strings.Repeat("c", 80), comment: "x"},
		{name: "an index name of 81 bytes", object: ydbcomment.Index, of: strings.Repeat("i", 81), comment: "x"},
		{name: "4096 bytes", object: ydbcomment.Table, comment: strings.Repeat("x", 4096)},
		{name: "2048 two-byte characters", object: ydbcomment.View, comment: strings.Repeat("é", 2048)},
		{name: "control characters", object: ydbcomment.Table, comment: "a\nb\x00c"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbcomment.Refusal(tc.object, tc.of, tc.comment), qt.Equals, "")
		})
	}
}

// A comment YDB cannot hold is refused with the limit it breaks. Ported from
// the host's TestRefusal_FailurePath.
func TestRefusal_FailurePath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		object  ydbcomment.Object
		of      string
		comment string
		want    string
	}{
		{name: "a column name of 81 bytes", object: ydbcomment.Column, of: strings.Repeat("c", 81), comment: "x",
			want: `YDB keeps the comment as the table attribute "ptah.comment.column.` + strings.Repeat("c", 81) +
				`", 101 bytes long, and an attribute key takes at most 100 bytes; a column name takes at most 80`},
		{name: "an index name of 82 bytes", object: ydbcomment.Index, of: strings.Repeat("i", 82),
			want: `YDB keeps the comment as the table attribute "ptah.comment.index.` + strings.Repeat("i", 82) +
				`", 101 bytes long, and an attribute key takes at most 100 bytes; an index name takes at most 81`},
		{name: "4097 bytes", object: ydbcomment.Table, comment: strings.Repeat("x", 4097),
			want: "the comment is 4097 bytes long, and YDB keeps at most 4096 bytes in an attribute value"},
		{name: "2049 two-byte characters", object: ydbcomment.Table, comment: strings.Repeat("é", 2049),
			want: "the comment is 4098 bytes long, and YDB keeps at most 4096 bytes in an attribute value"},
		{name: "not UTF-8", object: ydbcomment.Table, comment: "\xff\xfe",
			want: "the comment is not UTF-8 text, which YDB keeps in an attribute value"},
		{name: "a column with no name", object: ydbcomment.Column, comment: "x",
			want: "a column comment names no column"},
		{name: "an index with no name", object: ydbcomment.Index, comment: "x",
			want: "an index comment names no index"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(ydbcomment.Refusal(tc.object, tc.of, tc.comment), qt.Equals, tc.want)
		})
	}
}
