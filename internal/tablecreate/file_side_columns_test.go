package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/columnsemantics"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// fileSideRequest alters app.records, whose current side carries no column
// facet, as the side the host builds from a schema file for a diff of two
// files does, with the knowledge of the column facets given: the value column
// is held NOT NULL, or nullable with the common default held, and the
// declaration lets it take NULL with the default declared.
func fileSideRequest(c *qt.C, knowledge protocol.ModelKnowledge_State, notNull bool, held string, declared bool) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := alterRequest(c, alterCase{declared: []string{"value"}, held: []string{"value"}, heldTable: plainTable, capabilities: map[string]bool{"alter_column_default": true}})
	request.TargetFeatures = append(request.TargetFeatures, provider.ColumnsFeature)
	table := request.Tables[0]
	table.Current.Columns[0].DataType, table.Current.Columns[0].IsNullable = "Uint64", "NO"
	table.Current.Columns[1].DataType, table.Current.Columns[1].IsNullable = "Utf8", map[bool]string{true: "NO", false: "YES"}[notNull]
	table.Current.Columns[1].DefaultExpression = new(held)
	table.Desired.Columns[1].Nullable = true
	table.Desired.Columns[1].DefaultSet = declared
	var model *protocol.ValueModel
	for _, candidate := range schema.ObservedModels() {
		if candidate.Kind == schema.ColumnProperties {
			model = candidate
		}
	}
	table.Current.Features.Facets.Coverage.Kinds = append(table.Current.Features.Facets.Coverage.Kinds, &protocol.ComparisonKindCoverage{Model: model,
		Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: knowledge, Reason: "fixture"}}}})
	return request
}

// A NOT NULL column the declaration lets take NULL is changed in place where
// the current side states its column facets completely and carries none for
// the column, as the side the host builds from a schema file does: the
// column holds no native setting, and the operation holds the column as the
// common column says. The host's embedded engine planned the change offline.
func TestNullabilityPlanReadsAFileSide_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	plan, err := tablecreate.Plan(c.Context(), fileSideRequest(c, protocol.ModelKnowledge_COMPLETE, true, "", false))

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	change, err := tablecreate.DecodeNullability(plan.Contributions[0].Steps[0].Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(change.Held.GetName(), qt.Equals, "value")
	c.Assert(change.Held.GetFromLiteral(), qt.IsNil)
}

// A default changed in place reads the default the column holds from the
// common column of such a side, as the native literal that reads back as it.
func TestDefaultPlanReadsAFileSide_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	plan, err := tablecreate.Plan(c.Context(), fileSideRequest(c, protocol.ModelKnowledge_COMPLETE, false, "'old'u", true))

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	change, err := tablecreate.DecodeDefault(plan.Contributions[0].Steps[0].Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	held, err := columnsemantics.Literal(change.Held.GetFromLiteral())
	c.Assert(err, qt.IsNil)
	c.Assert(held, qt.Equals, "'old'u")
}

// A side that does not state its column facets says nothing about a column
// it carries no facet for, and the change is refused, as is one whose common
// default no native literal reads back as.
func TestColumnPlanWithoutTheNativeColumn_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name      string
		knowledge protocol.ModelKnowledge_State
		notNull   bool
		held      string
		declared  bool
		want      string
	}{
		{name: "a nullability change, the facets uninspected", knowledge: protocol.ModelKnowledge_UNINSPECTED, notNull: true,
			want: "A column nullability change requires the complete native column observation"},
		{name: "a default change, the facets uninspected", knowledge: protocol.ModelKnowledge_UNINSPECTED, held: "'old'u", declared: true,
			want: "A column default change requires the complete native column observation"},
		{name: "a default change, a common default that is no literal", knowledge: protocol.ModelKnowledge_COMPLETE, held: "CurrentUtcTimestamp()", declared: true,
			want: "A column default change requires the complete native column observation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			plan, err := tablecreate.Plan(c.Context(), fileSideRequest(c, test.knowledge, test.notNull, test.held, test.declared))

			c.Assert(err, qt.IsNil)
			c.Assert(plan.Contributions, qt.HasLen, 0)
			c.Assert(plan.Diagnostics, qt.HasLen, 1)
			c.Assert(plan.Diagnostics[0].Message, qt.Equals, test.want)
		})
	}
}
