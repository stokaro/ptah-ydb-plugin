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

// A plan made against no read of a database, such as the one an offline
// render makes, writes the table without a Serial's start and increment: the
// ALTER SEQUENCE names the sequence only by its absolute path, which begins
// with the database's own. The render reports both as omissions, as the host's
// embedded engine's renderer did for an offline `schema render` (measured with
// built-in 74de0b65b: the CREATE TABLE alone, and `identity increment=5` and
// `identity start=100` from `schema validate --no-skipped`), rather than
// refusing the whole render. The native creation states the settings in the
// call that creates the table. The embedded engine's migration planner
// refused such a plan (TestGenerateMigrationAST_SerialSequence_FailurePath); a
// migration is planned against its database, so the TEXT form here serves an
// offline render.
func TestSerialCreationWithoutItsDatabase_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request, plan := serialCreationIn(c, "")
	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT

	rendered, err := backend.New().Render(c.Context(), rendering)

	c.Assert(err, qt.IsNil)
	c.Assert(rendered.GetRefusal(), qt.IsNil)
	c.Assert(rendered.GetUnits(), qt.HasLen, 1)
	c.Assert(rendered.GetUnits()[0].GetSql(), qt.Equals, "CREATE TABLE `app/records` (\n    `id` BigSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);")
	c.Assert(omissionTexts(rendered.GetOmissions()), qt.DeepEquals, []string{
		"unsupported column app.records.id: identity increment=5",
		"unsupported column app.records.id: identity start=100",
	})
}

// omissionTexts writes each omission as its reason, kind, name, property and
// detail.
func omissionTexts(omissions []*protocol.RenderOmission) []string {
	texts := make([]string, 0, len(omissions))
	for _, omission := range omissions {
		texts = append(texts, omission.GetReason()+" "+omission.GetKind()+" "+omission.GetName()+": "+omission.GetProperty()+"="+omission.GetDetail())
	}
	return texts
}
