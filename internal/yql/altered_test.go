package yql_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

func TestAlteredTable_HappyPath(t *testing.T) {
	for _, test := range []struct{ text, table string }{
		{"ALTER TABLE t ADD INDEX i GLOBAL ON (v)", "t"},
		{"alter table `dir/t` ADD INDEX i GLOBAL ON (v);", "dir/t"},
		{"--!syntax_v1\n-- add\nALTER TABLE `/local/a\\`b` ADD COLUMN v Utf8", "/local/a`b"},
	} {
		t.Run(test.text, func(t *testing.T) {
			c := qt.New(t)

			table, ok := yql.AlteredTable(test.text)

			c.Assert(ok, qt.IsTrue)
			c.Assert(table, qt.Equals, test.table)
		})
	}
}

func TestAlteredTable_FailurePath(t *testing.T) {
	for _, text := range []string{
		"CREATE TABLE t (id Int64, PRIMARY KEY (id))",
		"ALTER TABLE $t ADD INDEX i GLOBAL ON (v)",
		"PRAGMA TablePathPrefix = \"/x\"; ALTER TABLE t ADD INDEX i GLOBAL ON (v)",
		"ALTER TABLE a ADD COLUMN v Utf8; ALTER TABLE b ADD COLUMN v Utf8",
		"ALTER TOPIC t ADD CONSUMER c",
		"ALTER TABLE",
		"ALTER TABLE `t ADD COLUMN v Utf8",
	} {
		t.Run(text, func(t *testing.T) {
			c := qt.New(t)

			table, ok := yql.AlteredTable(text)

			c.Assert(ok, qt.IsFalse)
			c.Assert(table, qt.Equals, "")
		})
	}
}
