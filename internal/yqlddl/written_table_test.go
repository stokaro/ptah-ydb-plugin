package yqlddl_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlddl"
)

// Each data statement names the table it writes rows into: after INTO for an
// INSERT, an INSERT OR REVERT, an UPSERT and a REPLACE, after UPDATE, and after
// DELETE FROM, each also behind BATCH and after a comment. Ported from the
// host's TestWrittenTable_HappyPath.
func TestWrittenTable_HappyPath(t *testing.T) {
	for _, tc := range []struct {
		statement string
		want      string
	}{
		{statement: "UPSERT INTO `docs/d` (id, emb) VALUES (1, \"x\")", want: "docs/d"},
		{statement: "insert into d (id) values (1);", want: "d"},
		{statement: "INSERT OR REVERT INTO d SELECT * FROM e", want: "d"},
		{statement: "REPLACE INTO d (id) VALUES (1)", want: "d"},
		{statement: "UPDATE d SET n = 1 WHERE id = 2", want: "d"},
		{statement: "BATCH UPDATE d SET n = 1", want: "d"},
		{statement: "DELETE FROM d WHERE id = 1", want: "d"},
		{statement: "-- a comment\nBATCH DELETE FROM `d` WHERE id = 1", want: "d"},
	} {
		t.Run(tc.statement, func(t *testing.T) {
			c := testcheck.New(t)
			got, ok := yqlddl.WrittenTable(tc.statement)
			c.Assert(ok, qt.IsTrue)
			c.Assert(got, qt.Equals, tc.want)
		})
	}
}

// A statement that writes no rows, a write into a named expression, which is
// not known before the query runs, a DELETE without FROM and empty text name
// no table. Ported from the host's TestWrittenTable_FailurePath.
func TestWrittenTable_FailurePath(t *testing.T) {
	for _, statement := range []string{
		"SELECT * FROM d",
		"ALTER TABLE d ADD COLUMN n Int64",
		"UPSERT INTO $target (id) VALUES (1)",
		"DELETE d",
		"",
	} {
		t.Run(statement, func(t *testing.T) {
			c := testcheck.New(t)
			got, ok := yqlddl.WrittenTable(statement)
			c.Assert(ok, qt.IsFalse)
			c.Assert(got, qt.Equals, "")
		})
	}
}
