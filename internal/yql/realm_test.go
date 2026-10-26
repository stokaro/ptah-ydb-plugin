package yql_test

import (
	"errors"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
)

func TestConfineToRealm_HappyPath(t *testing.T) {
	for _, test := range []struct{ name, source string }{
		{"empty", ""},
		{"comments only", "-- nothing\n/* here */"},
		{"relative table", "CREATE TABLE `app/t` (id Int64 NOT NULL, PRIMARY KEY (id));"},
		{"several statements", "CREATE TABLE a (id Int64 NOT NULL, PRIMARY KEY (id)); INSERT INTO a (id) VALUES (1); DROP TABLE a;"},
		{"escape in a comment", "-- DROP TABLE `/local/t`\nSELECT 1"},
		{"escape in a string", "INSERT INTO t (name) VALUES (\"DROP TABLE `/local/t`\")"},
		{"semicolon in a subquery body", "DEFINE SUBQUERY $s() AS SELECT 1; SELECT 2; END DEFINE; SELECT 3"},
		{"syntax setting", "--!syntax_v1\nSELECT 1"},
		{"unrelated pragma", "PRAGMA AnsiInForEmptyOrNullableItemsCollections; SELECT 1"},
		{"comment on a relative table", "COMMENT ON TABLE `app/t` IS 'text'"},
		{"comment on a relative column", "COMMENT ON COLUMN `app/t`.`id` IS 'text'"},
		{"comment on a relative index", "COMMENT ON INDEX `by_id` ON `app/t` IS NULL"},
		{"comment on a relative view", "CREATE VIEW `v` WITH (security_invoker = TRUE) AS SELECT 1;\nCOMMENT ON VIEW `v` IS 'text';"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(yql.ConfineToRealm(test.source), qt.IsNil)
		})
	}
}

func TestConfineToRealm_FailurePath(t *testing.T) {
	for _, test := range []struct{ name, source, operation string }{
		{"escape after a statement that stays", "CREATE TABLE a (id Int64 NOT NULL, PRIMARY KEY (id)); DROP TABLE `/local/a`", "the absolute path /local/a"},
		{"escape after a comment", "-- ok\nDROP TABLE `../a`", "the path ../a, which climbs out of the dev database"},
		{"escape in an action body", "DEFINE ACTION $a() AS DROP TABLE `/local/a`; END DEFINE; DO $a()", "DEFINE ACTION"},
		{"escape after an escaped backtick", "DROP TABLE `a``b/../..`", "the path a`b/../.., which climbs out of the dev database"},
		{"prefix moved after a statement", "SELECT 1; PRAGMA TablePathPrefix = \"/local\"", "PRAGMA TablePathPrefix"},
		{"user", "CREATE USER u1 PASSWORD 'p'", "a user of the whole database"},
		{"statement without a target", "INSERT VALUES (1)", "a INSERT with no INTO the guard can read"},
		{"translation setting", "--!ansi_lexer\nSELECT 1", "translation setting --!ansi_lexer"},
		{"comment on an absolute table", "COMMENT ON TABLE `/local/t` IS 'text'", "the absolute path /local/t"},
		{"comment on a column outside", "COMMENT ON COLUMN `../t`.`id` IS 'text'", "the path ../t, which climbs out of the dev database"},
		{"comment on an index outside", "COMMENT ON INDEX `i` ON `/local/t` IS 'text'", "the absolute path /local/t"},
		{"comment on another object", "COMMENT ON SCHEMA `app` IS 'text'", "unrecognized object COMMENT ON SCHEMA"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			err := yql.ConfineToRealm(test.source)

			var refusal *yql.RealmRefusal
			c.Assert(errors.As(err, &refusal), qt.IsTrue)
			c.Assert(refusal.Operation, qt.Equals, test.operation)
			c.Assert(err, qt.ErrorMatches, "YDB dev realm session rejects .* because its effects cannot be confined to the dev realm")
		})
	}
}
