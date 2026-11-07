package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// A column of a table that exists, declared to default to an expression, is
// refused: YDB keeps a default as a value, never as an expression evaluated
// for each row, and no YDB line has expression_defaults. Ported from the
// host's TestGenerateMigrationAST_RefusesByCapability_FailurePath ("an
// expression default").
func TestColumnAlterationRefusesAnExpressionDefault(t *testing.T) {
	c := testcheck.New(t)
	request := alterRequest(c, alterCase{declared: []string{"ts"}, types: map[string]string{"ts": "Timestamp"}, held: []string{"ts"}, heldTable: plainTable})
	request.TargetFeatures = append(request.TargetFeatures, provider.ColumnsFeature)
	table := request.Tables[0]
	table.Current.Columns[0].DataType, table.Current.Columns[0].IsNullable = "Uint64", "NO"
	table.Current.Columns[1].DataType, table.Current.Columns[1].IsNullable = "Timestamp", "YES"
	table.Desired.Columns[1].DefaultExpression = "CurrentUtcTimestamp()"

	plan, err := backend.New().PlanFeatures(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Contributions, qt.HasLen, 0)
	c.Assert(plan.Diagnostics, qt.HasLen, 1)
	c.Assert(plan.Diagnostics[0].Code, qt.Equals, protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE)
	c.Assert(plan.Diagnostics[0].Message, qt.Matches, `.*expression defaults require expression_defaults, unavailable on this YDB target`)
}
