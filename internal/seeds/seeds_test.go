package seeds_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/google/go-cmp/cmp/cmpopts"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/seeds"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
)

// The failures a fake transaction answers with, each read by the fake
// classifier as what a YDB failure of that kind proves.
var (
	errConflict = errors.New("conflict with existing key")
	errAborted  = errors.New("transaction locks invalidated")
	errRejected = errors.New("datashard program size limit exceeded")
	errLost     = errors.New("connection reset")
)

func classify(err error) seeds.Failure {
	return map[error]seeds.Failure{errConflict: seeds.Conflict, errAborted: seeds.Aborted, errRejected: seeds.Rejected}[err]
}

// script is what one attempt of a fake transaction answers: the failure of
// the statement at index fail (counted over the attempt's statements, the
// ledger row last), or of the commit.
type script struct {
	fail      int
	statement error
	commit    error
}

// fakeDatabase records what each attempt ran and answers from its scripts.
type fakeDatabase struct {
	scripts  []script
	begins   int
	ran      [][]string
	commits  int
	rollback int
	args     []any
}

type fakeTx struct {
	db     *fakeDatabase
	script script
}

func (db *fakeDatabase) begin(context.Context) (seeds.Tx, error) {
	attempt := script{fail: -1}
	if db.begins < len(db.scripts) {
		attempt = db.scripts[db.begins]
	}
	db.begins++
	db.ran = append(db.ran, nil)
	return &fakeTx{db: db, script: attempt}, nil
}

func (tx *fakeTx) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	ran := &tx.db.ran[len(tx.db.ran)-1]
	*ran = append(*ran, query)
	tx.db.args = args
	if len(*ran)-1 == tx.script.fail {
		return nil, tx.script.statement
	}
	return nil, nil
}

func (tx *fakeTx) Commit() error {
	tx.db.commits++
	return tx.script.commit
}

func (tx *fakeTx) Rollback() error {
	tx.db.rollback++
	return nil
}

func moment() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 5, time.FixedZone("CEST", 7200)) }

func seed() *protocol.Seed {
	return &protocol.Seed{Path: "seeds/regions.sql", Filename: "regions.sql", Env: "dev",
		Checksum: "0000000000000000000000000000000000000000000000000000000000000000"}
}

func split(c *qt.C, text string) []yqlquery.Query {
	c.Helper()
	queries, err := yqlquery.Split(text)
	c.Assert(err, qt.IsNil)
	return queries
}

func TestApply_HappyPath(t *testing.T) {
	c := qt.New(t)
	db := &fakeDatabase{}

	outcome := seeds.Apply(context.Background(), db.begin, classify,
		split(c, "$x = 1;\nUPSERT INTO t (id) VALUES ($x);\nUPSERT INTO t (id) VALUES (2);\n"), seed(), moment)

	c.Assert(outcome, qt.Equals, seeds.Outcome{Outcome: protocol.SeedResult_APPLIED})
	c.Assert(db.ran, qt.DeepEquals, [][]string{{"$x = 1;\nUPSERT INTO t (id) VALUES ($x);\nUPSERT INTO t (id) VALUES (2)", seeds.RecordStatement}})
	c.Assert(db.commits, qt.Equals, 1)
	c.Assert(db.args, qt.CmpEquals(cmpopts.EquateComparable(sql.NamedArg{})), []any{sql.Named("seed_path", "seeds/regions.sql"), sql.Named("env", "dev"),
		sql.Named("checksum", seed().GetChecksum()), sql.Named("applied_at", moment().UTC())})
}

// TestApplyRunsAnAbortedTransactionAgain_HappyPath pins that an abort YDB
// answered for a conflicting transaction, at a statement or at the commit,
// runs the whole transaction again from its first statement.
func TestApplyRunsAnAbortedTransactionAgain_HappyPath(t *testing.T) {
	c := qt.New(t)
	db := &fakeDatabase{scripts: []script{{fail: 1, statement: errAborted}, {fail: -1, commit: errAborted}, {fail: -1}}}

	outcome := seeds.Apply(context.Background(), db.begin, classify, split(c, "UPSERT INTO t (id) VALUES (1);"), seed(), moment)

	c.Assert(outcome, qt.Equals, seeds.Outcome{Outcome: protocol.SeedResult_APPLIED})
	c.Assert(db.begins, qt.Equals, 3)
	c.Assert(db.ran, qt.DeepEquals, [][]string{
		{"UPSERT INTO t (id) VALUES (1)", seeds.RecordStatement},
		{"UPSERT INTO t (id) VALUES (1)", seeds.RecordStatement},
		{"UPSERT INTO t (id) VALUES (1)", seeds.RecordStatement},
	})
	c.Assert(db.rollback, qt.Equals, 1)
}

func TestRecord_HappyPath(t *testing.T) {
	c := qt.New(t)
	db := &fakeDatabase{}

	outcome := seeds.Record(context.Background(), db.begin, classify, seed(), moment)

	c.Assert(outcome, qt.Equals, seeds.Outcome{Outcome: protocol.SeedResult_APPLIED})
	c.Assert(db.ran, qt.DeepEquals, [][]string{{seeds.RecordStatement}})
}

func TestApply_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name    string
		scripts []script
		want    seeds.Outcome
		begins  int
		commits int
	}{
		{
			name: "a statement hits an existing key", scripts: []script{{fail: 0, statement: errConflict}},
			want:   seeds.Outcome{Outcome: protocol.SeedResult_KEY_CONFLICT, Reason: "a statement of the seed hit an existing key, and nothing of the seed was applied"},
			begins: 1,
		},
		{
			name: "the commit hits an existing key", scripts: []script{{fail: -1, commit: errConflict}},
			want:   seeds.Outcome{Outcome: protocol.SeedResult_KEY_CONFLICT, Reason: "a statement of the seed hit an existing key, and nothing of the seed was applied"},
			begins: 1, commits: 1,
		},
		{
			name: "a statement fails", scripts: []script{{fail: 0, statement: errLost}},
			want:   seeds.Outcome{Outcome: protocol.SeedResult_NOT_APPLIED, Reason: "a statement of the seed failed: the connection failed; nothing of the seed was applied"},
			begins: 1,
		},
		{
			name: "the commit is rejected the same way again", scripts: []script{{fail: -1, commit: errRejected}},
			want:   seeds.Outcome{Outcome: protocol.SeedResult_NOT_APPLIED, Reason: "a statement of the seed failed: the connection failed; nothing of the seed was applied"},
			begins: 1, commits: 1,
		},
		{
			name: "the commit is lost", scripts: []script{{fail: -1, commit: errLost}},
			want:   seeds.Outcome{Outcome: protocol.SeedResult_UNKNOWN, Reason: "the seed's commit was not confirmed: the connection failed"},
			begins: 1, commits: 1,
		},
		{
			name: "every attempt is aborted",
			scripts: []script{{fail: 0, statement: errAborted}, {fail: 0, statement: errAborted}, {fail: 0, statement: errAborted},
				{fail: 0, statement: errAborted}, {fail: 0, statement: errAborted}, {fail: -1}},
			want:   seeds.Outcome{Outcome: protocol.SeedResult_NOT_APPLIED, Reason: "YDB aborted the seed's transaction for a conflicting one 5 times; nothing of the seed was applied"},
			begins: 5,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			db := &fakeDatabase{scripts: test.scripts}

			outcome := seeds.Apply(context.Background(), db.begin, classify, split(c, "UPSERT INTO t (id) VALUES (1);"), seed(), moment)

			c.Assert(outcome, qt.Equals, test.want)
			c.Assert(db.begins, qt.Equals, test.begins)
			c.Assert(db.commits, qt.Equals, test.commits)
		})
	}
}

func TestApplyStopsWhenTheBeginFails_FailurePath(t *testing.T) {
	c := qt.New(t)
	begin := func(context.Context) (seeds.Tx, error) { return nil, context.Canceled }

	outcome := seeds.Apply(context.Background(), begin, classify, split(c, "UPSERT INTO t (id) VALUES (1);"), seed(), moment)

	c.Assert(outcome, qt.Equals, seeds.Outcome{Outcome: protocol.SeedResult_NOT_APPLIED,
		Reason: "the seed's transaction could not begin: the request was canceled"})
}

func TestRefusal_HappyPath(t *testing.T) {
	for _, text := range []string{
		"UPSERT INTO t (id) VALUES (1);\nINSERT INTO t (id) VALUES (2);\n",
		"$x = 1;\nDECLARE $p AS Int64;\nUPSERT INTO t (id) VALUES ($x);\n",
		"--!syntax_v1\nUPSERT INTO t (id) VALUES (1);\n",
		"DEFINE ACTION $a($v) AS UPSERT INTO t (id) VALUES ($v); END DEFINE;\nDO $a(1);\n",
	} {
		t.Run(text, func(t *testing.T) {
			c := qt.New(t)

			offset, reason, refused := seeds.Refusal(text)

			c.Assert(refused, qt.IsFalse)
			c.Assert(reason, qt.Equals, "")
			c.Assert(offset, qt.Equals, 0)
		})
	}
}

func TestRefusal_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, text, reason string
		offset             int
	}{
		{
			name: "scheme", text: "UPSERT INTO t (id) VALUES (1);\nCREATE TABLE m (id Int64, PRIMARY KEY (id));\n",
			reason: "YDB runs a scheme query only outside a transaction, and a seed runs in one; move this statement to a migration",
			offset: 31,
		},
		{
			name: "batch after a definition", text: "UPSERT INTO t (id) VALUES (1);\n$x = 1;\nBATCH UPDATE t SET v = $x;\n",
			reason: "YDB runs a batch query only outside a transaction, and a seed runs in one; move this statement to a migration",
			offset: 39,
		},
		{
			name: "scheme after a definition that opens it", text: "$x = 1;\nCREATE TABLE m (id Int64, PRIMARY KEY (id));\n",
			reason: "YDB runs a scheme query only outside a transaction, and a seed runs in one; move this statement to a migration",
			offset: 0,
		},
		{
			name: "block", text: "DO BEGIN CREATE TABLE m (id Int64, PRIMARY KEY (id)); END DO;\n",
			reason: "YDB runs a scheme query only outside a transaction, and a seed runs in one; move this statement to a migration",
			offset: 0,
		},
		{
			name: "setting", text: "\n--!ansi_lexer\nSELECT 1;\n",
			reason: "unsupported YQL translation setting: YQL is split as --!syntax_v1 reads it, and YDB would read this text differently or refuse it; remove the line",
			offset: 1,
		},
		{
			name: "delimiter", text: "SELECT 1;\nDELIMITER $$\n",
			reason: "client delimiter directive in YQL: a delimiter directive has no meaning in YQL, which ends every statement with a semicolon; remove the directive and terminate each statement with a semicolon",
			offset: 10,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)

			offset, reason, refused := seeds.Refusal(test.text)

			c.Assert(refused, qt.IsTrue)
			c.Assert(reason, qt.Equals, test.reason)
			c.Assert(offset, qt.Equals, test.offset)
		})
	}
}

func ledgerColumn(name string, id Ydb.Type_PrimitiveTypeId, nullable bool) *native.ColumnMeta {
	column := &native.ColumnMeta{Name: name, Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: id}}}
	if nullable {
		column.Type = &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: column.Type}}}
	}
	return column
}

// ledger is the layout the host's tracker DDL creates, in its column order.
func ledger() *native.DescribeTableResult {
	return &native.DescribeTableResult{PrimaryKey: []string{"seed_path"}, Columns: []*native.ColumnMeta{
		ledgerColumn("seed_path", Ydb.Type_UTF8, false), ledgerColumn("env", Ydb.Type_UTF8, false),
		ledgerColumn("checksum", Ydb.Type_UTF8, false), ledgerColumn("applied_at", Ydb.Type_TIMESTAMP, false),
	}}
}

func TestCheckLayout_HappyPath(t *testing.T) {
	c := qt.New(t)
	reordered := ledger()
	reordered.Columns[0], reordered.Columns[3] = reordered.Columns[3], reordered.Columns[0]

	c.Assert(seeds.CheckLayout(ledger()), qt.IsNil)
	c.Assert(seeds.CheckLayout(reordered), qt.IsNil)
}

func TestCheckLayout_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*native.DescribeTableResult)
	}{
		{"nullable column", func(table *native.DescribeTableResult) {
			table.Columns[1] = ledgerColumn("env", Ydb.Type_UTF8, true)
		}},
		{"another type", func(table *native.DescribeTableResult) {
			table.Columns[3] = ledgerColumn("applied_at", Ydb.Type_DATETIME, false)
		}},
		{"another key", func(table *native.DescribeTableResult) { table.PrimaryKey = []string{"seed_path", "env"} }},
		{"a column more", func(table *native.DescribeTableResult) {
			table.Columns = append(table.Columns, ledgerColumn("extra", Ydb.Type_UTF8, true))
		}},
		{"a column renamed", func(table *native.DescribeTableResult) {
			table.Columns[2] = ledgerColumn("digest", Ydb.Type_UTF8, false)
		}},
		{"an index", func(table *native.DescribeTableResult) {
			table.Indexes = []*native.TableIndexDescription{{Name: "by_env"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			table := ledger()
			test.change(table)

			err := seeds.CheckLayout(table)

			c.Assert(err, qt.ErrorMatches, "the table schema_seeds does not have the seed ledger's layout, and it is not adopted")
		})
	}
}

// TestCreateStatementIsTheHostTracker pins the ledger's statement to the
// host's tracker statement for YDB (internal/builtindialects), so a ledger
// either engine created is the other's.
func TestCreateStatementIsTheHostTracker(t *testing.T) {
	c := qt.New(t)

	c.Assert(seeds.CreateStatement, qt.Equals, "CREATE TABLE IF NOT EXISTS `schema_seeds` (\n"+
		"    seed_path Utf8 NOT NULL,\n    env Utf8 NOT NULL,\n    checksum Utf8 NOT NULL,\n"+
		"    applied_at Timestamp NOT NULL,\n    PRIMARY KEY (seed_path)\n)")
}
