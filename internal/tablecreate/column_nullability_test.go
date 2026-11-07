package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb"
	"google.golang.org/protobuf/proto"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// notNullValue is column value as a NOT NULL Utf8 column.
func notNullValue() *native.ColumnMeta {
	return &native.ColumnMeta{Name: "value", Type: &Ydb.Type{Type: &Ydb.Type_TypeId{TypeId: Ydb.Type_UTF8}}}
}

// A nullability operation carries the NOT NULL column it changes and writes
// the DROP NOT NULL the host's embedded engine wrote. Ported from the host's
// DROP NOT NULL rows of TestGenerateMigrationAST_ColumnChanges_HappyPath.
func TestNullabilityOperation_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	change := &tablecreate.NullabilityAlteration{Held: notNullValue()}

	operation, err := tablecreate.CaptureNullability("nullability", owner(), change)

	c.Assert(err, qt.IsNil)
	c.Assert(operation.OperationSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
	decoded, err := tablecreate.DecodeNullability(operation.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(decoded.Held, change.Held), qt.IsTrue)
	c.Assert(tablecreate.NullabilityStatement("app/records", decoded), qt.Equals, "ALTER TABLE `app/records` ALTER COLUMN `value` DROP NOT NULL;")
}

// A nullability operation refuses a column that already takes NULL, and one
// it cannot name.
func TestNullabilityOperation_FailurePath(t *testing.T) {
	optional := notNullValue()
	optional.Type = &Ydb.Type{Type: &Ydb.Type_OptionalType{OptionalType: &Ydb.OptionalType{Item: optional.Type}}}
	unnamed := notNullValue()
	unnamed.Name = ""
	for _, test := range []struct {
		name string
		held *native.ColumnMeta
	}{
		{name: "a column that takes NULL", held: optional},
		{name: "a column without a name", held: unnamed},
		{name: "no column"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			operation, err := tablecreate.CaptureNullability("nullability", owner(), &tablecreate.NullabilityAlteration{Held: test.held})

			c.Assert(err, qt.IsNotNil)
			c.Assert(operation, qt.IsNil)
		})
	}
}

// nullabilityRequest alters app.records, whose column value the table holds
// NOT NULL, into a declaration of it that takes NULL, or not.
func nullabilityRequest(c *qt.C, nullable bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := alterRequest(c, alterCase{declared: []string{"value"}, held: []string{"value"}, heldTable: plainTable, heldColumns: map[string]string{"value": `{}`}})
	request.TargetFeatures = append(request.TargetFeatures, provider.ColumnsFeature)
	table := request.Tables[0]
	table.Current.Columns[0].DataType, table.Current.Columns[0].IsNullable = "Uint64", "NO"
	table.Current.Columns[1].DataType, table.Current.Columns[1].IsNullable = "Utf8", "NO"
	table.Desired.Columns[1].Nullable = nullable
	return request
}

// A NOT NULL column the declaration lets take NULL is changed in place by one
// owned step that reads the table and alters the column, as the host's
// embedded engine planned it.
func TestNullabilityPlan_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	plan, err := tablecreate.Plan(c.Context(), nullabilityRequest(c, true))

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	step := plan.Contributions[0].Steps[0]
	c.Assert(step.Operation.Value.Value.Kind, qt.Equals, tablecreate.NullabilityOperationKind)
	c.Assert(step.Metadata.Transaction, qt.Equals, protocol.PlanningMetadata_FORBIDDEN)
	c.Assert(step.Metadata.Effects[0].Action, qt.Equals, protocol.PlanningEffect_READ)
	c.Assert(step.Metadata.Effects[1].Action, qt.Equals, protocol.PlanningEffect_ALTER)
	c.Assert(step.Metadata.Effects[1].Subject.Name.Source, qt.Equals, "value")
}

// A declaration that keeps the column NOT NULL plans nothing for it.
func TestNullabilityPlanKeepsANotNullColumn(t *testing.T) {
	c := testcheck.New(t)

	plan, err := tablecreate.Plan(c.Context(), nullabilityRequest(c, false))

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 0)
}
