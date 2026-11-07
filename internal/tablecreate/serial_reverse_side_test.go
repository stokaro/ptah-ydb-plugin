package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/ydbsequence"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// projectedSerialRequest alters app.records, whose key id the current side
// holds as a BigSerial stepping by held, with no column facet and its column
// facets stated completely, as the host's reverse plan builds the side of a
// column its forward plan changed, in a plan of the database at database.
// The declaration steps by declared.
func projectedSerialRequest(c *qt.C, held, declared, database string) *protocol.PlanFeaturesRequest {
	c.Helper()
	request := alterRequest(c, alterCase{heldTable: plainTable, capabilities: serialCapabilities()})
	request.TargetFeatures = append(request.TargetFeatures, provider.ColumnsFeature)
	request.DatabasePath = database
	table := request.Tables[0]
	table.Desired.Columns = []*protocol.DeclaredColumn{{Name: "id", Type: "BIGSERIAL", Primary: true, IdentityIncrement: declared}}
	table.Current.Columns = []*protocol.ObservedColumn{{Name: "id", DataType: "Int64", IsNullable: "NO", PrimaryKey: true, AutoIncrement: true, IdentityIncrement: held}}
	var model *protocol.ValueModel
	for _, candidate := range schema.ObservedModels() {
		if candidate.Kind == schema.ColumnProperties {
			model = candidate
		}
	}
	table.Current.Features.Facets.Coverage.Kinds = append(table.Current.Features.Facets.Coverage.Kinds, &protocol.ComparisonKindCoverage{Model: model,
		Sources: []*protocol.ComparisonKnowledgeSource{{Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE, Reason: "fixture"}}}})
	return request
}

// A side that states its column facets completely and carries none for a
// Serial column holds the sequence the common column describes, so the
// rollback of an increment change reads the increment the forward change
// left and changes it back without a restart (stokaro/ptah#5735). The
// operation carries the database the plan read, which the TEXT form names
// the sequence under, as the host names the planned table by no database.
func TestSequencePlanReadsAProjectedSerial_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	plan, err := tablecreate.Plan(c.Context(), projectedSerialRequest(c, "10", "5", "/local"))

	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	change, err := tablecreate.DecodeSequence(plan.Contributions[0].Steps[0].Operation.Value.Value)
	c.Assert(err, qt.IsNil)
	c.Assert(change.Held.GetFromSequence().GetIncrement(), qt.Equals, int64(10))
	c.Assert(change.Settings, qt.Equals, ydbsequence.Settings{Start: 1, Increment: 5})
	c.Assert(change.Database, qt.Equals, "/local")
	text, err := tablecreate.SequenceAlterationStatement("", "app/records", change)
	c.Assert(err, qt.IsNil)
	c.Assert(text, qt.Equals, "ALTER SEQUENCE `/local/app/records/_serial_column_id` START WITH 1 INCREMENT BY 5;")
}

// A sequence change planned against no read of a database names no database
// and still has no TEXT form.
func TestSequencePlanReadsAProjectedSerial_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	plan, err := tablecreate.Plan(c.Context(), projectedSerialRequest(c, "10", "5", ""))
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	change, err := tablecreate.DecodeSequence(plan.Contributions[0].Steps[0].Operation.Value.Value)
	c.Assert(err, qt.IsNil)

	text, err := tablecreate.SequenceAlterationStatement("", "app/records", change)

	c.Assert(err, qt.ErrorIs, tablecreate.ErrNoTextForm)
	c.Assert(text, qt.Equals, "")
}
