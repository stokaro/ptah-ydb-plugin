package yql_test

import (
	"context"
	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/yql"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"strings"
	"testing"
)

func TestAnalysisRefusesWholeBatch(t *testing.T) {
	for _, source := range []string{
		"SELECT 'unterminated", "SELECT \"bad\\x1\"", "SELECT @@unterminated", "SELECT `unterminated", "/* unterminated", "SELECT 1; /*unterminated",
		"DEFINE ACTION $a() AS SELECT 1; DROP TABLE secret;", "$x = () -> { SELECT 1;", "SELECT };", "SELECT 1\x00; DROP TABLE secret", string([]byte{0xff}),
		"--!ansi_lexer\nSELECT \"x\"", "SELECT 1; --!ansi_lexer\nSELECT \"x\"", "--!syntax_v0\nSELECT 1", "-- comment only", "; ;",
	} {
		t.Run(source, func(t *testing.T) {
			c := qt.New(t)
			result, err := yql.Analyze(c.Context(), &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{"CREATE TABLE t (id Int64, PRIMARY KEY(id));", source}})
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(result, qt.IsNil)
		})
	}
}

func TestAnalysisRetainsTriviaOffsetsAndTokens(t *testing.T) {
	c := qt.New(t)
	source := "  -- heading\nCREATE/* gap */TABLE `Case` (id Int64, PRIMARY KEY(id));\n-- ending\n; "
	request := &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{source, " \t\n"}}
	result, err := yql.Analyze(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateSQLAnalysis(request, result), qt.IsNil)
	c.Assert(result.Files, qt.HasLen, 2)
	c.Assert(result.Files[0].Statements, qt.HasLen, 1)
	statement := result.Files[0].Statements[0]
	c.Assert(statement.Start, qt.Equals, uint32(2))
	c.Assert(statement.End, qt.Equals, uint32(len(source)))
	c.Assert(statement.Line, qt.Equals, uint32(1))
	c.Assert(statement.SourceWords[2], qt.Equals, "`Case`")
	c.Assert(statement.Words[2], qt.Equals, "`Case`")
	c.Assert(statement.Canonical, qt.Contains, "CREATE TABLE `Case`")
	c.Assert(result.Files[1].Statements, qt.HasLen, 0)
}

func TestAnalysisNativeSafety(t *testing.T) {
	tests := []struct {
		sql  string
		want protocol.SQLStatement_Safety
	}{
		{"CREATE TABLE t (id Int64, PRIMARY KEY(id));", protocol.SQLStatement_COMMON_DDL},
		{"ALTER TABLE t ADD COLUMN v Utf8;", protocol.SQLStatement_COMMON_DDL},
		{"DROP TABLE t", protocol.SQLStatement_DESTRUCTIVE},
		{"ALTER TABLE t DROP COLUMN v", protocol.SQLStatement_DESTRUCTIVE},
		{"DROP TOPIC topic", protocol.SQLStatement_DESTRUCTIVE},
		{"ALTER TOPIC topic DROP CONSUMER consumer", protocol.SQLStatement_DESTRUCTIVE},
		{"DROP TRANSFER transfer", protocol.SQLStatement_DESTRUCTIVE},
		{"DROP COORDINATION NODE node", protocol.SQLStatement_DESTRUCTIVE},
		{"DROP SECRET secret", protocol.SQLStatement_DESTRUCTIVE},
		{"DROP ASYNC REPLICATION r CASCADE", protocol.SQLStatement_DESTRUCTIVE},
		{"DROP ASYNC REPLICATION r", protocol.SQLStatement_BEHAVIORAL},
		{"DROP EXTERNAL TABLE t", protocol.SQLStatement_BEHAVIORAL},
		{"DROP EXTERNAL DATA SOURCE s", protocol.SQLStatement_BEHAVIORAL},
		{"CREATE OR REPLACE EXTERNAL TABLE t (id Int64)", protocol.SQLStatement_BEHAVIORAL},
		{"DROP STREAMING QUERY q", protocol.SQLStatement_DESTRUCTIVE},
		{"CREATE OR REPLACE STREAMING QUERY q AS DO BEGIN SELECT 1; END DO;", protocol.SQLStatement_DESTRUCTIVE},
		{"ALTER STREAMING QUERY q AS DO BEGIN SELECT 1; END DO;", protocol.SQLStatement_DESTRUCTIVE},
		{"ALTER STREAMING QUERY q SET (RUN = FALSE)", protocol.SQLStatement_BEHAVIORAL},
		{"ALTER TABLE t SET (TTL = Interval(\"P1D\") ON created)", protocol.SQLStatement_UNKNOWN},
		{"ALTER TABLE t ADD COLUMN v Utf8, DROP CHANGEFEED feed", protocol.SQLStatement_UNKNOWN},
		{"DEFINE ACTION $a() AS DROP TABLE t; END DEFINE;", protocol.SQLStatement_UNKNOWN},
		{"DO $a()", protocol.SQLStatement_UNKNOWN},
		{"UPSERT INTO t (id) VALUES (1)", protocol.SQLStatement_DESTRUCTIVE},
		{"SELECT 'DROP TABLE t'", protocol.SQLStatement_UNKNOWN},
		{"SELECT `DROP` FROM t", protocol.SQLStatement_UNKNOWN},
		{"GRANT ALL ON t TO user", protocol.SQLStatement_UNKNOWN},
	}
	for _, test := range tests {
		t.Run(test.sql, func(t *testing.T) {
			c := qt.New(t)
			result, err := yql.Analyze(c.Context(), &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{test.sql}})
			c.Assert(err, qt.IsNil)
			c.Assert(result.Files[0].Statements, qt.HasLen, 1)
			statement := result.Files[0].Statements[0]
			c.Assert(statement.Safety, qt.Equals, test.want)
			if test.want != protocol.SQLStatement_COMMON_DDL {
				c.Assert(statement.Reason, qt.Not(qt.Equals), "")
			}
		})
	}
}

func TestAnalysisLimitsAndCancellation(t *testing.T) {
	c := qt.New(t)
	canceled, cancel := context.WithCancel(c.Context())
	cancel()
	result, err := yql.Analyze(canceled, &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{"SELECT 1"}})
	c.Assert(status.Code(err), qt.Equals, codes.Canceled)
	c.Assert(result, qt.IsNil)
	for _, request := range []*protocol.AnalyzeSQLRequest{nil, {Engine: "foreign"}, {Engine: "ydb", Sources: make([]string, 257)}, {Engine: "ydb", Sources: []string{strings.Repeat("x", 3<<20)}}, {Engine: "ydb", Sources: []string{strings.Repeat("x ", 600000)}}} {
		result, err := yql.Analyze(c.Context(), request)
		c.Assert(err, qt.IsNotNil)
		c.Assert(result, qt.IsNil)
	}
}

func FuzzAnalysisFraming(f *testing.F) {
	for _, source := range []string{"SELECT 1;", "/*comment*/SELECT @@a;@@@@b@@;", "DEFINE ACTION $a() AS SELECT 1; END DEFINE; DO $a();", "SELECT 'bad", "--!ansi_lexer\nSELECT 1"} {
		f.Add(source)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 65536 {
			t.Skip()
		}
		c := qt.New(t)
		request := &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{source}}
		result, err := yql.Analyze(c.Context(), request)
		if err != nil {
			c.Assert(result, qt.IsNil)
			return
		}
		c.Assert(provider.ValidateSQLAnalysis(request, result), qt.IsNil)
	})
}
