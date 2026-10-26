package ydbview_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbview"
)

// storedQueries are view queries as a CREATE VIEW sent them and as
// DescribeView returned them, measured on 25.1.4.7 and 26.2.1.14 alike; the
// table path is shortened to `t`.
var storedQueries = []struct{ name, body, stored string }{
	{"comparisons",
		"SELECT a>=1 AS x1, a<>2 AS x2, a!=3 AS x3, a==4 AS x4, a<=5 AS x5, a<6 AS x6, a>7 AS x7, a=8 AS x8 FROM `t`",
		"SELECT a >= 1 AS x1 , a <> 2 AS x2 , a != 3 AS x3 , a == 4 AS x4 , a <= 5 AS x5 , a < 6 AS x6 , a > 7 AS x7 , a = 8 AS x8 FROM `t`"},
	{"bit operators",
		"SELECT u<<1 AS l, u>>1 AS r, u|1u AS o, u&1u AS n, u^1u AS x, ~u AS t, a%2 AS m, -a AS neg, u|<<1 AS rl, u>>|1 AS rr FROM `t`",
		"SELECT u << 1 AS l , u > > 1 AS r , u | 1u AS o , u & 1u AS n , u ^ 1u AS x , ~ u AS t , a % 2 AS m , - a AS neg , u |<< 1 AS rl , u > > | 1 AS rr FROM `t`"},
	{"numbers",
		"SELECT 1e+5 AS e1, 1.5f AS f, 10u AS u, 0x1F AS h, 2.5e-3 AS e2, 7l AS lg, 3ut AS ut, 1.0 AS dec, 0.5 AS half, 1E3 AS big, 0b101 AS bin, 0o17 AS oct FROM `t`",
		"SELECT 1e+5 AS e1 , 1.5f AS f , 10u AS u , 0x1F AS h , 2.5e-3 AS e2 , 7l AS lg , 3ut AS ut , 1.0 AS dec , 0.5 AS half , 1E3 AS big , 0b101 AS bin , 0o17 AS oct FROM `t`"},
	{"strings",
		"SELECT s||'x' AS c, 'q'u AS q1, \"dq\" AS q2, @@multi\nline@@ AS ml, 'it\\'s' AS q3 FROM `t`",
		"SELECT s || 'x' AS c , 'q'u AS q1 , \"dq\" AS q2 , @@multi\nline@@ AS ml , 'it\\'s' AS q3 FROM `t`"},
	{"namespaces, members, optional types and structs",
		"SELECT Math::Pi() AS pi, AsStruct(1 AS f).f AS fld, CAST(a AS Int32?) AS opt, <|k:1|> AS st FROM `t`",
		"SELECT Math :: Pi ( ) AS pi , AsStruct ( 1 AS f ) . f AS fld , CAST ( a AS Int32 ? ) AS opt , <| k : 1 |> AS st FROM `t`"},
	{"comments, case, quoted names and terminators",
		"select   a as `we``ird`, a AS `x y` -- comment\n /* block */ from `t` where a in (1,2) order by a desc limit 5;;",
		"select a as `we``ird` , a AS `x y` from `t` where a in ( 1 , 2 ) order by a desc limit 5"},
	{"keywords", "SELECT a FROM `t` WHERE s LIKE 'a%' ESCAPE '!' AND a BETWEEN 1 AND 2",
		"SELECT a FROM `t` WHERE s LIKE 'a%' ESCAPE '!' AND a BETWEEN 1 AND 2"},
	{"lists and dictionaries", "SELECT [1,2][0] AS li, {'a':1} AS di FROM `t`",
		"SELECT [ 1 , 2 ] [ 0 ] AS li , { 'a' : 1 } AS di FROM `t`"},
	{"lambdas and named expressions", "SELECT ListMap([1,2], ($x) -> { RETURN $x + 1; }) AS lm FROM `t`",
		"SELECT ListMap ( [ 1 , 2 ] , ( $ x ) -> { RETURN $ x + 1 ; } ) AS lm FROM `t`"},
	{"non-ASCII text", "SELECT 'привет' AS greet, a AS `имя` FROM `t`", "SELECT 'привет' AS greet , a AS `имя` FROM `t`"},
	{"qualified columns", "SELECT t.a, t.`s` FROM `t` AS t", "SELECT t . a , t . `s` FROM `t` AS t"},
	{"aggregates", "SELECT COUNT(*) AS c FROM `t` GROUP BY a HAVING COUNT(*)>1;",
		"SELECT COUNT ( * ) AS c FROM `t` GROUP BY a HAVING COUNT ( * ) > 1"},
	{"signs and escapes", "SELECT a FROM `t` WHERE a IS NOT NULL AND d > -1.5e10 AND s != \"\\x41\"",
		"SELECT a FROM `t` WHERE a IS NOT NULL AND d > - 1.5e10 AND s != \"\\x41\""},
	{"trailing comment", "SELECT a FROM `t` WHERE a = 1 -- trailing comment", "SELECT a FROM `t` WHERE a = 1"},
}

func TestQueryText_HappyPath(t *testing.T) {
	for _, tc := range storedQueries {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(ydbview.QueryText(tc.body), qt.Equals, tc.stored)
		})
		t.Run(tc.name+" read back", func(t *testing.T) {
			c := qt.New(t)
			c.Assert(ydbview.QueryText(tc.stored), qt.Equals, tc.stored)
		})
	}
}

func TestQueryText_Edges(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"empty", "", ""},
		{"only trivia", "  -- c\n/* d */ ;; ", ""},
		{"unterminated block comment", "SELECT 1 /* open", "SELECT 1"},
		{"unterminated string", "SELECT 'open", "SELECT 'open"},
		{"escaped multi-line delimiter", "SELECT @@a@@@@b@@ AS s", "SELECT @@a@@@@b@@ AS s"},
		{"shift right then comparison", "SELECT u>>=1", "SELECT u > >= 1"},
		{"hexadecimal e", "SELECT 0x1E+5", "SELECT 0x1E + 5"},
		{"inner semicolon kept", "SELECT ($x) -> { RETURN $x; };", "SELECT ( $ x ) -> { RETURN $ x ; }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(ydbview.QueryText(tc.body), qt.Equals, tc.want)
		})
	}
}

func TestRealmQueryText(t *testing.T) {
	for _, tc := range []struct{ name, body, root, want string }{
		{"own realm", "PRAGMA TablePathPrefix(\"/local/ptah_dev/r1\");\nSELECT id FROM t", "/local/ptah_dev/r1", "SELECT id FROM t"},
		{"another realm", "PRAGMA TablePathPrefix(\"/local/ptah_dev/r2\");\nSELECT id FROM t", "/local/ptah_dev/r1",
			"PRAGMA TablePathPrefix(\"/local/ptah_dev/r2\");\nSELECT id FROM t"},
		{"author's pragma", "PRAGMA TablePathPrefix('/local/app');\nSELECT id FROM t", "/local/app", "PRAGMA TablePathPrefix('/local/app');\nSELECT id FROM t"},
		{"no pragma", "SELECT id FROM t", "/local/ptah_dev/r1", "SELECT id FROM t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := qt.New(t)
			c.Assert(ydbview.RealmQueryText(tc.body, tc.root), qt.Equals, tc.want)
		})
	}
}

func TestRealmPrefix(t *testing.T) {
	c := qt.New(t)
	c.Assert(ydbview.RealmPrefix("/lo\"c\\al"), qt.Equals, "PRAGMA TablePathPrefix(\"/lo\\\"c\\\\al\");\n")
}
