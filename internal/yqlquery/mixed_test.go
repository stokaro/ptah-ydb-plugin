package yqlquery_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/yqlquery"
)

// A query YDB refuses whole is marked as one, and its neighbors are not: the
// call of an action the text defined to write and create, and a loop that
// writes and drops, are marked; a block of scheme statements and the
// definition of the action are not. Ported from the host's
// TestSplit_MarksAMixedQuery.
func TestSplit_MarksAMixedQuery(t *testing.T) {
	c := testcheck.New(t)
	text := "CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id));\n" +
		"DEFINE ACTION $a() AS UPSERT INTO t (id) VALUES (5l); CREATE TABLE n (id Int64 NOT NULL, PRIMARY KEY (id)); END DEFINE;\n" +
		"UPSERT INTO t (id) VALUES (1l);\n" +
		"DO $a();\n" +
		"DO BEGIN CREATE TABLE o (id Int64 NOT NULL, PRIMARY KEY (id)); END DO;\n" +
		"EVALUATE FOR $i IN AsList(7, 8) DO BEGIN UPSERT INTO t (id) VALUES ($i); DROP TABLE m; END DO;"

	queries, err := yqlquery.Split(text)

	c.Assert(err, qt.IsNil)
	mixed := make([]bool, 0, len(queries))
	for _, query := range queries {
		mixed = append(mixed, query.Mixed)
	}
	c.Assert(mixed, qt.DeepEquals, []bool{false, false, true, false, true})
}

// mixedAfter reads every statement but the last through one Reader, and
// answers whether the last one mixes scheme and table statements in a query.
func mixedAfter(statements []string) bool {
	var reader yqlquery.Reader
	for _, statement := range statements[:len(statements)-1] {
		reader.Mixed(statement)
	}
	return reader.Mixed(statements[len(statements)-1])
}

// Each of these YDB refuses whole, measured by the host on 26.2.1.14 and
// 25.1.4.7, with `Queries with mixed data and scheme operations are not
// supported`. Ported from the host's TestReader_Mixed_RefusedQueries.
func TestReader_Mixed_RefusedQueries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statements []string
	}{
		{name: "a block that writes and creates", statements: []string{
			"DO BEGIN UPSERT INTO t (id) VALUES (1l); CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); END DO"}},
		{name: "a block that reads a table and creates", statements: []string{
			"DO BEGIN SELECT * FROM t; CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); END DO"}},
		{name: "a block that deletes and alters", statements: []string{
			"DO BEGIN DELETE FROM t WHERE id = 99; ALTER TABLE t ADD COLUMN w Utf8; END DO"}},
		{name: "a loop that writes and drops", statements: []string{
			"EVALUATE FOR $i IN AsList(7, 8) DO BEGIN UPSERT INTO t (id) VALUES ($i); DROP TABLE m; END DO"}},
		{name: "a call of an action that writes and creates", statements: []string{
			"DEFINE ACTION $a() AS UPSERT INTO t (id) VALUES (5l); CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); END DEFINE",
			"DO $a()"}},
		{name: "a block that creates and calls an action that writes", statements: []string{
			"DEFINE ACTION $w() AS UPSERT INTO t (id) VALUES (5l); END DEFINE",
			"DO BEGIN CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); DO $w(); END DO"}},
		{name: "a read through a join", statements: []string{
			"DO BEGIN SELECT * FROM AS_TABLE($rows) AS r JOIN `dir/t` AS t ON r.id = t.id; DROP TABLE m; END DO"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(mixedAfter(tc.statements), qt.IsTrue)
		})
	}
}

// Each of these YDB runs, measured by the host on 26.2.1.14 and 25.1.4.7, or
// it is not a mixed query at all. Ported from the host's
// TestReader_Mixed_AcceptedQueries.
func TestReader_Mixed_AcceptedQueries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statements []string
	}{
		{name: "a block whose SELECT reads no table", statements: []string{
			"DO BEGIN SELECT 1; CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); END DO"}},
		{name: "a block whose SELECT reads a list", statements: []string{
			"DO BEGIN SELECT * FROM AS_TABLE(AsList(AsStruct(1 AS a))); CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); END DO"}},
		{name: "a block with a named expression", statements: []string{
			"DO BEGIN $x = 1; CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); END DO"}},
		{name: "a block of writes", statements: []string{
			"DO BEGIN UPSERT INTO t (id) VALUES (1l); DELETE FROM t WHERE id = 2l; END DO"}},
		{name: "a block of scheme statements", statements: []string{
			"DO BEGIN CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); DROP TABLE n; END DO"}},
		{name: "a write and a scheme statement apart, which the split runs as two queries", statements: []string{
			"UPSERT INTO t (id) VALUES (1l)",
			"CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id))"}},
		{name: "the definition of a mixed action, which runs nothing until it is called", statements: []string{
			"DEFINE ACTION $a() AS UPSERT INTO t (id) VALUES (5l); CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); END DEFINE"}},
		{name: "a call of an action the text did not define", statements: []string{
			"DO BEGIN CREATE TABLE m (id Int64 NOT NULL, PRIMARY KEY (id)); DO $elsewhere(); END DO"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(mixedAfter(tc.statements), qt.IsFalse)
		})
	}
}
