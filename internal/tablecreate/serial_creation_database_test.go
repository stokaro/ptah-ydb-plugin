package tablecreate_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecreate"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// serialCreationText plans the creation of app.records with key id as the
// column given, in a plan of the database at database whose subject names no
// database, as the host names a table it plans, and renders the creation in
// the TEXT form. It returns the statement and the payload members of the
// creation beside the request's own.
func serialCreationText(c *qt.C, database string, id *protocol.DeclaredColumn) (string, map[string]json.RawMessage) {
	c.Helper()
	request := creationPlan(c)
	request.DatabasePath = database
	request.Capabilities = serialCapabilities()
	request.Capabilities["transactional_ddl"] = false
	request.Tables[0].Subject.Database = nil
	table := serialTable(id)
	table.Features = request.Tables[0].Desired.Features
	table.Features.Facets.Records = nil
	request.Tables[0].Desired = table
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	rendering := testplanning.RenderRequest(backend.Description(), plan, request.Capabilities)
	rendering.Form, rendering.TargetFeatures = protocol.RenderRequest_TEXT, []string{provider.TextUnitsFeature}
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	var payload map[string]json.RawMessage
	c.Assert(json.Unmarshal(plan.Contributions[0].Steps[0].Operation.Value.Value.Payload, &payload), qt.IsNil)
	return rendered.Units[0].GetSql(), payload
}

// The host names a table it plans by no database, so the creation of a table
// whose Serial gets a start or an increment carries the path of the database
// the plan read, and its TEXT form, the up file of a migration, gives the
// sequence its settings with the ALTER SEQUENCE the host's embedded engine
// wrote. Without the path the file created the sequence with YDB's start and
// increment of 1 (G238). A creation whose Serial keeps YDB's settings carries
// no path, and a plan of no database writes the table alone, as an offline
// render does.
func TestSerialCreationTextNamesThePlansDatabase_HappyPath(t *testing.T) {
	table := "CREATE TABLE `app/records` (\n    `id` BigSerial NOT NULL,\n    `note` Utf8,\n    PRIMARY KEY (`id`)\n);"
	for _, test := range []struct {
		name, database, want, carried string
		id                            *protocol.DeclaredColumn
	}{
		{name: "a start and an increment", database: "/local", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "5"},
			want: table + "\nALTER SEQUENCE `/local/app/records/_serial_column_id` START WITH 100 INCREMENT BY 5 RESTART WITH 100;", carried: `"/local"`},
		{name: "YDB's own settings", database: "/local", id: &protocol.DeclaredColumn{Type: "BIGSERIAL"}, want: table},
		{name: "a plan of no database", id: &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100", IdentityIncrement: "5"}, want: table},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			text, payload := serialCreationText(c, test.database, test.id)

			c.Assert(text, qt.Equals, test.want)
			c.Assert(string(payload["database"]), qt.Equals, test.carried)
		})
	}
}

// A creation names its database by a clean absolute path, or it is refused.
func TestSerialCreationDatabase_FailurePath(t *testing.T) {
	for _, database := range []string{`"local"`, `"/local/../other"`, `7`} {
		t.Run(database, func(t *testing.T) {
			c := testcheck.New(t)
			_, payload := serialCreationText(c, "/local", &protocol.DeclaredColumn{Type: "BIGSERIAL", IdentityStart: "100"})
			payload["database"] = json.RawMessage(database)
			forged, err := json.Marshal(payload)
			c.Assert(err, qt.IsNil)
			value := tablecreate.Model()

			normalized, err := tablecreate.Normalize(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
				ProviderId: schema.ProviderID, Kind: tablecreate.OperationKind, SchemaVersion: 1, Encoding: value.Encoding,
				DefinitionSha256: value.DefinitionSha256, Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "records"}, Payload: forged}})

			c.Assert(err, qt.ErrorMatches, "a YDB table creation names its database by a clean absolute path")
			c.Assert(normalized, qt.IsNil)
		})
	}
}
