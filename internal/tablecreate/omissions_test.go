package tablecreate_test

import (
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
)

// modifiedColumns are the columns of app.records whose declared types YDB
// keeps only in part, beside the key and a column it keeps whole.
func modifiedColumns() []*protocol.DeclaredColumn {
	return []*protocol.DeclaredColumn{{Name: "id", Type: "BIGINT", Primary: true}, {Name: "email", Type: "VARCHAR(255)", Nullable: true},
		{Name: "seen", Type: "TIMESTAMP(3)", Nullable: true}, {Name: "note", Type: "TEXT", Nullable: true}}
}

// modifiedCreation plans the creation of app.records with modifiedColumns
// and returns the render request of the plan.
func modifiedCreation(c *qt.C) *protocol.RenderRequest {
	c.Helper()
	request := creationPlan(c)
	table := request.Tables[0]
	table.Desired.PrimaryKey, table.Desired.Columns, table.Desired.Features.Facets.Records = []string{"id"}, modifiedColumns(), nil
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	return testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
}

// A render of a table's creation reports the part of each column's declared
// type YDB does not keep, in either form: the length of a VARCHAR(255), whose
// Utf8 has none, and a timestamp precision, where YDB keeps microseconds. The
// host's embedded engine reported them in these words, naming the column by
// its table's directory and name, for `schema render` and `schema validate
// --no-skipped` (G219; measured with built-in 74de0b65b). TEXT, which YDB
// keeps whole, reports nothing.
func TestRenderReportsTypeModifiers_HappyPath(t *testing.T) {
	for _, form := range []protocol.RenderRequest_Form{protocol.RenderRequest_ANY, protocol.RenderRequest_TEXT} {
		t.Run(form.String(), func(t *testing.T) {
			c := testcheck.New(t)
			rendering := modifiedCreation(c)
			rendering.Form = form

			rendered, err := backend.New().Render(c.Context(), rendering)

			c.Assert(err, qt.IsNil)
			c.Assert(rendered.GetUnits(), qt.HasLen, 1)
			c.Assert(omissionTexts(rendered.GetOmissions()), qt.DeepEquals, []string{
				"unsupported column app.records.email: type modifier=VARCHAR(255): length 255",
				"unsupported column app.records.seen: type modifier=TIMESTAMP(3): precision 3 (YDB keeps microseconds)",
			})
		})
	}
}

// A host that does not list render.omissions gets the same units and no
// omission, as a host of contract 25.32 gets none.
func TestRenderReportsTypeModifiersWithoutTheFeature_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	rendering := modifiedCreation(c)
	rendering.TargetFeatures = slices.DeleteFunc(slices.Clone(rendering.TargetFeatures), func(feature string) bool {
		return feature == provider.RenderOmissionsFeature
	})

	rendered, err := backend.New().Render(c.Context(), rendering)

	c.Assert(err, qt.IsNil)
	c.Assert(rendered.GetUnits(), qt.HasLen, 1)
	c.Assert(rendered.GetOmissions(), qt.HasLen, 0)
}

// A column table's creation carries the type modifiers of its columns too,
// and a creation without one carries none.
func TestCreationOmissions_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	columnTable := columnDeclaration(c, `{}`)
	columnTable.Columns = append(columnTable.Columns, &protocol.DeclaredColumn{Name: "code", Type: "CHAR(8)", Nullable: true})
	plain := &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Int64", Primary: true}}}

	column, err := tablecreate.CaptureColumnTable("create", owner(), columnTable, nil)
	c.Assert(err, qt.IsNil)
	row, err := tablecreate.Capture("create", owner(), plain, make(map[string]bool))
	c.Assert(err, qt.IsNil)
	columnOmissions, columnErr := tablecreate.CreationOmissions(column.GetValue())
	rowOmissions, rowErr := tablecreate.CreationOmissions(row.GetValue())

	c.Assert(columnErr, qt.IsNil)
	c.Assert(columnOmissions, qt.DeepEquals, []tablecreate.Omission{{Column: "code", Property: "type modifier", Detail: "CHAR(8): length 8"}})
	c.Assert(rowErr, qt.IsNil)
	c.Assert(rowOmissions, qt.HasLen, 0)
}

// A creation whose type modifiers name a column it does not create, or name
// one twice, does not normalize: the payload is not one the provider wrote.
func TestCreationOmissions_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name      string
		modifiers string
		want      string
	}{
		{name: "an unknown column", modifiers: `[{"column":"other","property":"type modifier","detail":"VARCHAR(9): length 9"}]`,
			want: "YDB creation type modifiers name no created column"},
		{name: "a column twice", modifiers: `[{"column":"id","property":"type modifier","detail":"a"},{"column":"id","property":"type modifier","detail":"b"}]`,
			want: "invalid YDB creation type modifiers"},
		{name: "another property", modifiers: `[{"column":"id","property":"identity start","detail":"100"}]`,
			want: "invalid YDB creation type modifiers"},
		{name: "a detail of two lines", modifiers: `[{"column":"id","property":"type modifier","detail":"a\nb"}]`,
			want: "invalid YDB creation type modifiers"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			created, err := tablecreate.Capture("create", owner(), &protocol.DeclaredTable{Name: "records", Schema: "app", PrimaryKey: []string{"id"},
				Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "Int64", Primary: true}}}, make(map[string]bool))
			c.Assert(err, qt.IsNil)
			forged := proto.CloneOf(created.GetValue())
			forged.Payload = append(forged.Payload[:len(forged.Payload)-1], []byte(`,"typeModifiers":`+test.modifiers+`}`)...)

			normalized, err := tablecreate.Normalize(&protocol.ValueInput{Id: "create", Representation: protocol.ValueInput_OPERATION, Value: forged})

			c.Assert(err, qt.ErrorMatches, test.want)
			c.Assert(normalized, qt.IsNil)
		})
	}
}
