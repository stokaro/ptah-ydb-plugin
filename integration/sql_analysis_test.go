//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

func TestYQLAnalysisOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	c.Assert(client.Description().Features, qt.Contains, provider.AnalyzeSQLFeature)
	source := "-- heading\nCREATE TABLE `app/t` (id Int64, PRIMARY KEY(id));\nDROP/* x */TOPIC `topic`; -- trailing"
	request := &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{source}}
	result, err := client.AnalyzeSQL(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateSQLAnalysis(request, result), qt.IsNil)
	c.Assert(result.Files[0].Statements, qt.HasLen, 2)
	c.Assert(result.Files[0].Statements[0].Safety, qt.Equals, protocol.SQLStatement_COMMON_DDL)
	c.Assert(result.Files[0].Statements[1].Safety, qt.Equals, protocol.SQLStatement_DESTRUCTIVE)
	c.Assert(source[result.Files[0].Statements[1].Start:result.Files[0].Statements[1].End], qt.Equals, "DROP/* x */TOPIC `topic`; -- trailing")
	refused, err := client.AnalyzeSQL(c.Context(), &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{source, "SELECT 'unclosed"}})
	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.IsNil)
	// Analysis uses no database session. A canceled call does not poison it.
	canceled, cancel := context.WithCancel(c.Context())
	cancel()
	_, err = client.AnalyzeSQL(canceled, request)
	c.Assert(err, qt.IsNotNil)
	_, err = client.AnalyzeSQL(c.Context(), request)
	c.Assert(err, qt.IsNil)
}

func TestYQLAnalysisMatchesLiveQueries(t *testing.T) {
	c := qt.New(t)
	opened, _, _ := inspectionFixture(c)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	cases := []struct {
		name, source string
		statements   int
		want         []string
	}{
		{"quoted strings", `SELECT "a;b"u; SELECT 'c\'d'u;`, 2, []string{"a;b", "c'd"}},
		{"multiline", `SELECT @@a;@@@@b@@u; SELECT @@c@@@u;`, 2, []string{"a;@@b", "c@"}},
		{"inline action", "DO BEGIN SELECT 1; SELECT 2; END DO; SELECT 3;", 2, []string{"1", "2", "3"}},
		{"defined action", "DEFINE ACTION $a() AS SELECT 4; END DEFINE; DO $a();", 2, []string{"4"}},
		{"lambda", "$f = ($x) -> { $y = $x + 1; RETURN $y; }; SELECT $f(4);", 2, []string{"5"}},
		{"quoted name", "SELECT 1 AS `a;``b`; SELECT 2;", 2, []string{"1", "2"}},
		{"number suffix", "SELECT 1uAS value; SELECT 2;", 2, []string{"1", "2"}},
		{"comment boundaries", "SELECT 1 /* ; */; -- note ;\nSELECT 2;", 2, []string{"1", "2"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			ctx, cancel := context.WithTimeout(c.Context(), time.Minute)
			defer cancel()
			request := &protocol.AnalyzeSQLRequest{Engine: "ydb", Sources: []string{test.source}}
			result, err := client.AnalyzeSQL(ctx, request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateSQLAnalysis(request, result), qt.IsNil)
			statements := result.Files[0].Statements
			c.Assert(statements, qt.HasLen, test.statements)
			var reconstructed strings.Builder
			for _, statement := range statements {
				reconstructed.WriteString(test.source[statement.Start:statement.End])
				reconstructed.WriteByte('\n')
			}
			rows, err := opened.DB.QueryContext(ctx, reconstructed.String())
			c.Assert(err, qt.IsNil)
			defer func() { c.Check(rows.Close(), qt.IsNil) }()
			var got []string
			for {
				for rows.Next() {
					var value string
					c.Assert(rows.Scan(&value), qt.IsNil)
					got = append(got, value)
				}
				c.Assert(rows.Err(), qt.IsNil)
				if !rows.NextResultSet() {
					break
				}
			}
			c.Assert(rows.Err(), qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
		})
	}
}
