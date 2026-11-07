package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
)

// serialCreationIn plans the creation of app.records, whose key id is a
// BigSerial starting at 100 and stepping by 5, in database, empty for a plan
// made against no read of a database.
func serialCreationIn(c *qt.C, database string) (*protocol.PlanFeaturesRequest, *protocol.PlannedFeatures) {
	c.Helper()
	request := creationPlan(c)
	request.DatabasePath = database
	request.Capabilities = serialCapabilities()
	request.Capabilities["transactional_ddl"] = false
	table := request.Tables[0]
	table.Subject.Database = &protocol.ComparisonPart{Source: database, Normalized: database}
	declared := serialTable(&protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "5"})
	table.Desired.PrimaryKey, table.Desired.Columns, table.Desired.Features.Facets.Records = declared.PrimaryKey, declared.Columns, nil
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	return request, plan
}

// A plan made against a database writes a Serial's start and increment with
// the ALTER SEQUENCE that names the sequence by its absolute path, after the
// CREATE TABLE. Ported from the host's
// TestGenerateMigrationAST_SerialSequence_FailurePath, whose refusal this is
// the control of.
func TestSerialCreationNamesTheSequenceInItsDatabase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request, plan := serialCreationIn(c, "/local")

	statements := renderedText(c, plan, request.Capabilities)

	c.Assert(statements, qt.DeepEquals, []string{"CREATE TABLE `app/records` (\n    `id` BigSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);\n" +
		"ALTER SEQUENCE `/local/app/records/_serial_column_id` START WITH 100 INCREMENT BY 5 RESTART WITH 100;"})
}

// A plan made against no read of a database cannot write a Serial's start
// and increment as statements: the ALTER SEQUENCE names the sequence only by
// its absolute path, which begins with the database's own. The TEXT form is
// refused, rather than written with a path that names another sequence. The
// native creation states the settings in the call that creates the table, and
// execution binds the database there. Ported from the host's
// TestGenerateMigrationAST_SerialSequence_FailurePath ("a plan made against
// no read of a database").
func TestSerialCreationNamesTheSequenceInItsDatabase_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request, plan := serialCreationIn(c, "")
	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT

	rendered, err := backend.New().Render(c.Context(), rendering)

	c.Assert(err, qt.IsNil)
	c.Assert(rendered.GetUnits(), qt.HasLen, 0)
	c.Assert(rendered.GetRefusal().GetChangeId(), qt.Equals, plan.Contributions[0].Steps[0].Operation.Value.Id)
	c.Assert(rendered.GetRefusal().GetReason(), qt.Equals, protocol.RenderRefusal_NO_TEXT_FORM)
	c.Assert(rendered.GetRefusal().GetMessage(), qt.Matches, `.*the sequence of column "id" takes its start and increment from an ALTER SEQUENCE, `+
		`which names the sequence by its absolute path, and the creation names no database`)
}
