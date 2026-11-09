package migrationunits_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/migrationunits"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// The corpus is internal/yqlquery's: YQL texts and the queries the host's
// internal/yqlquery cut each into, with the source of each, or its error,
// recorded by internal/rowdata/testdata/oracle. A unit is one of those
// queries, so the split must give the host's text and source for each.

type corpusQuery struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

type corpusRow struct {
	name    string
	body    string
	error   string
	queries []corpusQuery
}

// placements is where the host runs a query of each kind the corpus records.
var placements = map[string]protocol.MigrationUnit_Placement{
	"data":   protocol.MigrationUnit_WITH_CHECKPOINT,
	"scheme": protocol.MigrationUnit_OUTSIDE,
	"batch":  protocol.MigrationUnit_OUTSIDE,
}

// refusedByTheMigrator are the corpus texts the host's yqlquery splits and
// its migrator refuses before a run (refuseUnsplittableSQL): the text the
// refusal names, and why.
var refusedByTheMigrator = map[string]struct{ refused, reason string }{
	"action":              {"DO $a();", migrationunits.MixedReason},
	"mixed-block":         {"DO BEGIN UPSERT INTO t (id) VALUES (1); CREATE TABLE m (id Int64, PRIMARY KEY (id)); END DO;", migrationunits.MixedReason},
	"transaction-control": {"COMMIT;", migrationunits.TransactionControlReason},
}

func corpus(c *qt.C) []corpusRow {
	c.Helper()
	paths, err := filepath.Glob("../yqlquery/testdata/corpus/*.golden.json")
	c.Assert(err, qt.IsNil)
	slices.Sort(paths)
	rows := make([]corpusRow, 0, len(paths))
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".golden.json")
		data, err := os.ReadFile(path)
		c.Assert(err, qt.IsNil)
		var golden struct {
			Error   string        `json:"error"`
			Queries []corpusQuery `json:"queries"`
		}
		c.Assert(json.Unmarshal(data, &golden), qt.IsNil)
		body, err := os.ReadFile(filepath.Join(filepath.Dir(path), name+".yql"))
		c.Assert(err, qt.IsNil)
		rows = append(rows, corpusRow{name: name, body: string(body), error: golden.Error, queries: golden.Queries})
	}
	return rows
}

// splitRows are the corpus texts the host runs as queries.
func splitRows(c *qt.C) []corpusRow {
	c.Helper()
	return slices.DeleteFunc(corpus(c), func(row corpusRow) bool {
		_, refused := refusedByTheMigrator[row.name]
		return row.error != "" || refused
	})
}

// settingRows are the corpus texts the host's yqlquery refuses.
func settingRows(c *qt.C) []corpusRow {
	c.Helper()
	return slices.DeleteFunc(corpus(c), func(row corpusRow) bool { return row.error == "" })
}

func TestSplitRunsTheHostsQueries_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	rows := splitRows(c)
	c.Assert(len(rows) >= 19, qt.IsTrue, qt.Commentf("%d split texts", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)
			want := make([]*protocol.MigrationUnit, len(row.queries))
			for i, query := range row.queries {
				want[i] = &protocol.MigrationUnit{Sql: query.Text, Source: query.Source, Placement: placements[query.Kind]}
			}

			units, refusal := migrationunits.Split(row.body)

			c.Assert(refusal, qt.IsNil)
			c.Assert(units, qt.CmpEquals(protocmp.Transform()), want)
		})
	}
}

func TestSplitRefusesWhatTheHostRefuses_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	rows := slices.DeleteFunc(corpus(c), func(row corpusRow) bool {
		_, refused := refusedByTheMigrator[row.name]
		return !refused
	})
	c.Assert(rows, qt.HasLen, len(refusedByTheMigrator))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)

			units, refusal := migrationunits.Split(row.body)

			c.Assert(units, qt.IsNil)
			c.Assert(row.body[refusal.GetStart():refusal.GetEnd()], qt.Equals, refusedByTheMigrator[row.name].refused)
			c.Assert(refusal.GetReason(), qt.Equals, refusedByTheMigrator[row.name].reason)
		})
	}
}

// TestSplitRefusesTheHostsUnreadableText_FailurePath compares the refused
// range with the text the host's refusal quotes.
func TestSplitRefusesTheHostsUnreadableText_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	rows := settingRows(c)
	c.Assert(len(rows) >= 3, qt.IsTrue, qt.Commentf("%d refused texts", len(rows)))
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := testcheck.New(t)

			units, refusal := migrationunits.Split(row.body)

			c.Assert(units, qt.IsNil)
			c.Assert(row.error, qt.Contains, strconv.Quote(row.body[refusal.GetStart():refusal.GetEnd()]))
			c.Assert(refusal.GetReason(), qt.Not(qt.Contains), row.body[refusal.GetStart():refusal.GetEnd()])
		})
	}
}

// TestSplitCarriesDefinitionsAndPlacesBlocks pins units the corpus does not
// spell: definitions carried past a scheme statement, a data block run with a
// checkpoint, and a scheme block or loop run outside a transaction.
func TestSplitCarriesDefinitionsAndPlacesBlocks(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want []*protocol.MigrationUnit
	}{
		{
			name: "carried name",
			body: "CREATE TABLE t (id Uint64, v Utf8, PRIMARY KEY (id));\n$v = \"a\"u;\nUPSERT INTO t (id, v) VALUES (1u, $v);\nUPSERT INTO t (id, v) VALUES (2u, $v);\n",
			want: []*protocol.MigrationUnit{
				{Sql: "CREATE TABLE t (id Uint64, v Utf8, PRIMARY KEY (id))", Source: "CREATE TABLE t (id Uint64, v Utf8, PRIMARY KEY (id));", Placement: protocol.MigrationUnit_OUTSIDE},
				{
					Sql:       "$v = \"a\"u;\nUPSERT INTO t (id, v) VALUES (1u, $v);\nUPSERT INTO t (id, v) VALUES (2u, $v)",
					Source:    "$v = \"a\"u;\nUPSERT INTO t (id, v) VALUES (1u, $v);\nUPSERT INTO t (id, v) VALUES (2u, $v);",
					Placement: protocol.MigrationUnit_WITH_CHECKPOINT,
				},
			},
		},
		{
			name: "data block",
			body: "DO BEGIN UPSERT INTO t (id) VALUES (1u); END DO;",
			want: []*protocol.MigrationUnit{{Sql: "DO BEGIN UPSERT INTO t (id) VALUES (1u); END DO", Source: "DO BEGIN UPSERT INTO t (id) VALUES (1u); END DO;", Placement: protocol.MigrationUnit_WITH_CHECKPOINT}},
		},
		{
			name: "scheme loop",
			body: "DEFINE ACTION $make($name) AS\n  CREATE TABLE $name (id Int64 NOT NULL, PRIMARY KEY (id));\nEND DEFINE;\nEVALUATE FOR $t IN AsList('b', 'c') DO $make($t);\n",
			want: []*protocol.MigrationUnit{{
				Sql:       "DEFINE ACTION $make($name) AS\n  CREATE TABLE $name (id Int64 NOT NULL, PRIMARY KEY (id));\nEND DEFINE;\nEVALUATE FOR $t IN AsList('b', 'c') DO $make($t)",
				Source:    "DEFINE ACTION $make($name) AS\n  CREATE TABLE $name (id Int64 NOT NULL, PRIMARY KEY (id));\nEND DEFINE;\nEVALUATE FOR $t IN AsList('b', 'c') DO $make($t);",
				Placement: protocol.MigrationUnit_OUTSIDE,
			}},
		},
		{
			name: "batch",
			body: "--!syntax_v1\n$n = 0l;\nBATCH UPDATE t SET n = $n WHERE id > 0l;",
			want: []*protocol.MigrationUnit{{Sql: "--!syntax_v1\n$n = 0l;\nBATCH UPDATE t SET n = $n WHERE id > 0l", Source: "$n = 0l;\nBATCH UPDATE t SET n = $n WHERE id > 0l;", Placement: protocol.MigrationUnit_OUTSIDE}},
		},
		{
			name: "no statement",
			body: "-- nothing runs\n;\n",
			want: make([]*protocol.MigrationUnit, 0),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			units, refusal := migrationunits.Split(test.body)

			c.Assert(refusal, qt.IsNil)
			c.Assert(units, qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

// TestSplitRefusesTheBody_FailurePath pins each refusal with the bytes it
// names: what the host's embedded YDB engine refuses before a run.
func TestSplitRefusesTheBody_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		refused string
		reason  string
	}{
		{"ansi lexer", "--!ansi_lexer\nSELECT 1;", "--!ansi_lexer", "unsupported YQL translation setting"},
		{"pg syntax", "--!syntax_pg  \nSELECT 1;", "--!syntax_pg", "unsupported YQL translation setting"},
		{"atlas delimiter", "SELECT 1;\n-- atlas:delimiter $$ \t\nSELECT 2$$", "-- atlas:delimiter $$", "client delimiter directive in YQL"},
		{"delimiter statement", "DELIMITER $$  \nSELECT 1$$", "DELIMITER $$", "client delimiter directive in YQL"},
		{"commit", "UPSERT INTO t (id) VALUES (1);\nCOMMIT;\n", "COMMIT;", migrationunits.TransactionControlReason},
		{"rollback in a run", "UPSERT INTO t (id) VALUES (1);\n  ROLLBACK ;", "ROLLBACK ;", migrationunits.TransactionControlReason},
		{"begin", "BEGIN TRANSACTION;", "BEGIN TRANSACTION;", migrationunits.TransactionControlReason},
		{"start", "START TRANSACTION", "START TRANSACTION", migrationunits.TransactionControlReason},
		{"savepoint", "CREATE TABLE m (id Int64, PRIMARY KEY (id));\nSAVEPOINT s;", "SAVEPOINT s;", migrationunits.TransactionControlReason},
		{"set transaction", "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE;", "SET TRANSACTION ISOLATION LEVEL SERIALIZABLE;", migrationunits.TransactionControlReason},
		{"mixed block", "CREATE TABLE a (id Int64, PRIMARY KEY (id));\nDO BEGIN SELECT * FROM t; DROP TABLE a; END DO;", "DO BEGIN SELECT * FROM t; DROP TABLE a; END DO;", migrationunits.MixedReason},
		{"mixed loop", "EVALUATE FOR $i IN AsList(7, 8) DO BEGIN UPSERT INTO t (id) VALUES ($i); DROP TABLE m; END DO;", "EVALUATE FOR $i IN AsList(7, 8) DO BEGIN UPSERT INTO t (id) VALUES ($i); DROP TABLE m; END DO;", migrationunits.MixedReason},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			units, refusal := migrationunits.Split(test.body)

			c.Assert(units, qt.IsNil)
			c.Assert(test.body[refusal.GetStart():refusal.GetEnd()], qt.Equals, test.refused)
			c.Assert(refusal.GetReason(), qt.Contains, test.reason)
		})
	}
}

func TestControlsTransaction(t *testing.T) {
	for _, test := range []struct {
		statement string
		want      bool
	}{
		{"COMMIT", true}, {"commit", true}, {"ROLLBACK", true}, {"ABORT", true}, {"END", true}, {"XA START 'x'", true},
		{"BEGIN", true}, {"SAVE TRANSACTION s", true}, {"SAVE TRAN s", true}, {"PREPARE TRANSACTION 'x'", true},
		{"RELEASE SAVEPOINT s", true}, {"START TRANSACTION", true}, {"SET IMPLICIT_TRANSACTIONS ON", true},
		{"-- note\nCOMMIT", true},
		{"SELECT 1", false}, {"UPSERT INTO commit (id) VALUES (1)", false}, {"SAVE x", false}, {"PREPARE x", false},
		{"RELEASE x", false}, {"START x", false}, {"SET x = 1", false}, {"DO BEGIN UPSERT INTO t (id) VALUES (1); END DO", false},
		{"`COMMIT`", false}, {"", false},
	} {
		t.Run(test.statement, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(migrationunits.ControlsTransaction(test.statement), qt.Equals, test.want)
		})
	}
}
