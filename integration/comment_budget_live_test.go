//go:build integration

package integration_test

import (
	"context"
	"path"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/connection"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-sdk/v3/table"
	"github.com/ydb-platform/ydb-go-sdk/v3/table/options"
)

// foreignAttributes gives the table at absolute user attributes another tool
// set.
func foreignAttributes(c *qt.C, opened *connection.Connection, absolute string, attributes map[string]string) {
	c.Helper()
	var alterations []options.AlterTableOption
	for key, value := range attributes {
		alterations = append(alterations, options.WithAddAttribute(key, value))
	}
	c.Assert(opened.Driver.Table().Do(c.Context(), func(ctx context.Context, session table.Session) error {
		return session.AlterTable(ctx, absolute, alterations...)
	}), qt.IsNil)
}

// Ported from the host's TestYDBComments_AttributesShareOneBudget: a
// comment is a user attribute of its table, which shares YDB's attribute
// budget with every other one. Attributes another tool set fill most of it,
// and YDB refuses the comment that would pass it, BAD_REQUEST with `user
// attributes too big`, in one AlterTable it applies whole or not at all. So a
// unit that is the comment, or whose comment follows a query YDB refused
// first, applied nothing and is NOT_APPLIED with the server's words, where
// both read UNKNOWN; a unit whose comment follows a query that applied is
// partly applied, and stays UNKNOWN. A statement run alone fails with the
// server's words. The attributes stay as they were.
func TestCommentAttributesShareOneBudgetSubprocess(t *testing.T) {
	c := testcheck.New(t)
	f := newCommentFixture(c)
	records := path.Join(f.database, f.directory, "records")
	foreignAttributes(c, f.opened, records, map[string]string{"a": strings.Repeat("a", 4096), "b": strings.Repeat("b", 4096)})
	quoted := "`" + f.directory + "/records`"
	comment := "COMMENT ON COLUMN " + quoted + ".`payload` IS '" + strings.Repeat("c", 2100) + "'"
	for _, test := range []struct{ name, unit, message string }{
		{name: "a comment YDB refuses", unit: comment + ";", message: `(?s).*user attributes too big.*`},
		{name: "a comment after a query YDB refused", unit: "CREATE VIEW `" + f.directory + "/v` WITH (security_invoker = TRUE) AS SELECT id FROM `" +
			f.directory + "/absent`;\nCOMMENT ON VIEW `" + f.directory + "/v` IS 'ids';", message: `(?s).*absent.*`},
	} {
		result, err := f.statement(c, test.unit)
		c.Assert(err, qt.ErrorIs, provider.ErrExecution, qt.Commentf("%s", test.name))
		c.Assert(result.Outcome, qt.Equals, protocol.Execution_NOT_APPLIED, qt.Commentf("%s: %s", test.name, result))
		c.Assert(result.DiagnosticCode, qt.Equals, "ydb.statement-rejected", qt.Commentf("%s", test.name))
		c.Assert(result.Message, qt.Matches, test.message, qt.Commentf("%s", test.name))
	}
	partly, err := f.statement(c, "UPSERT INTO "+quoted+" (id) VALUES (1ul);\n"+comment+";")
	c.Assert(err, qt.ErrorIs, provider.ErrOutcomeUnknown)
	c.Assert(partly.Outcome, qt.Equals, protocol.Execution_UNKNOWN, qt.Commentf("%s", partly))
	c.Assert(partly.DiagnosticCode, qt.Equals, "ydb.execution-unconfirmed")
	statement, err := f.client.RunStatement(c.Context(), &protocol.RunStatementRequest{SessionId: f.session.Id, RequestId: "budget", Sql: comment, Access: protocol.RunStatementRequest_READ_WRITE, MaxRows: 1})
	c.Assert(err, qt.IsNil)
	c.Assert(statement.Outcome, qt.Equals, protocol.StatementResult_FAILED, qt.Commentf("%s", statement))
	c.Assert(statement.ErrorCode, qt.Equals, "BAD_REQUEST")
	c.Assert(statement.Error, qt.Matches, `(?s).*user attributes too big.*`)

	held := storedAttributes(c, f.opened, records)
	c.Assert(held, qt.HasLen, 2)
	c.Assert(held["a"], qt.HasLen, 4096)
	c.Assert(rowCount(c, f.opened, records, ""), qt.Equals, int64(1))
}
