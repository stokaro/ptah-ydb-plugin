package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"
)

// hostSubject is the identity the host gives a table or a column: it names no
// database, since the table lives in the session's.
func hostSubject(identity *protocol.ComparisonIdentity) *protocol.ComparisonIdentity {
	result := proto.CloneOf(identity)
	result.Database = part("")
	return result
}

// hostRecord is record as the host hands it over. A declared facet is
// normalized with an owner that names no database, as the host's source
// declares it, and a read facet keeps the session's database the provider's
// inspection gave its owner.
func hostRecord(c *qt.C, record *protocol.ComparisonRecord, representation protocol.ValueInput_Representation) *protocol.ComparisonRecord {
	c.Helper()
	result := proto.CloneOf(record)
	result.Subject = hostSubject(record.Subject)
	c.Assert(record.Value.Value.Owner.Database, qt.Equals, "/local")
	if representation == protocol.ValueInput_OBSERVED {
		return result
	}
	envelope := proto.CloneOf(record.Value.Value)
	envelope.Owner.Database = ""
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb",
		Values: []*protocol.ValueInput{{Id: record.Value.Id, Representation: representation, Value: envelope}}})
	c.Assert(err, qt.IsNil)
	result.Value = values.Values[0]
	return result
}

// hostAlteration is the ALTER of app.records the host plans after comparing
// it: the request alterRequest captures, with the host's identities, and the
// changes the comparison of its facets reported.
func hostAlteration(c *qt.C, in alterCase) (*protocol.CompareModelsRequest, *protocol.PlanFeaturesRequest) {
	c.Helper()
	request := alterRequest(c, in)
	table := request.Tables[0]
	table.Subject = hostSubject(table.Subject)
	for _, state := range []*protocol.ComparisonState{table.Desired.Features.Facets, table.Current.Features.Facets} {
		for i, record := range state.Records {
			state.Records[i] = hostRecord(c, record, state.Representation)
		}
	}
	comparison := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: tablecompare.ServiceID, Kinds: tablecompare.Kinds(),
		Identifiers: proto.CloneOf(request.Identifiers), Desired: proto.CloneOf(table.Desired.Features.Facets), Current: proto.CloneOf(table.Current.Features.Facets),
		Parents: []*protocol.ComparisonParent{{Subject: proto.CloneOf(table.Subject), Desired: true, Current: true}}}
	return comparison, request
}

// TestHostIdentitiesCompareAndPlan compares and plans a TTL change of a table
// the host names without its database, whose read facets carry the session's
// database and whose declared ones carry none. Both sides name one table, so
// the comparison reports the change and the planner carries it out.
func TestHostIdentitiesCompareAndPlan(t *testing.T) {
	c := qt.New(t)
	comparison, planning := hostAlteration(c, ttlCase(ttlTable(hourly), plainTable, "ts"))
	compared, err := backend.New().CompareModels(c.Context(), comparison)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedModels(backend.Description(), comparison, compared), qt.IsNil)
	c.Assert(compared.Undecided, qt.HasLen, 0)
	c.Assert(compared.Changes, qt.HasLen, 1)
	c.Assert(compared.Changes[0].Value.Value.Owner.Database, qt.Equals, "")
	for _, change := range compared.Changes {
		planning.Changes = append(planning.Changes, &protocol.PlanningChange{Subject: change.Subject, Value: change.Value})
	}
	plan, err := backend.New().PlanFeatures(c.Context(), planning)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeatures(backend.Description(), planning, plan), qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	c.Assert(plan.Contributions, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps, qt.HasLen, 1)
	c.Assert(plan.Contributions[0].Steps[0].Id.Name, qt.Equals, "row-table-ttl/000000")
	c.Assert(plan.Contributions[0].Steps[0].Operation.Value.Value.Owner.Database, qt.Equals, "")
}

// TestHostIdentitiesCompareEqualFacets finds nothing to change in a table the
// host names without its database when both sides hold the same facets.
func TestHostIdentitiesCompareEqualFacets(t *testing.T) {
	c := qt.New(t)
	comparison, _ := hostAlteration(c, alterCase{desiredTable: ttlTable(hourly), heldTable: ttlTable(hourly), declared: []string{"ts", "note"}, types: ttlTypes,
		held: []string{"ts", "note"}, desiredColumns: map[string]string{"note": `{}`}, heldColumns: map[string]string{"note": `{}`}})
	compared, err := backend.New().CompareModels(c.Context(), comparison)
	c.Assert(err, qt.IsNil)
	c.Assert(compared.Undecided, qt.HasLen, 0)
	c.Assert(compared.Changes, qt.HasLen, 0)
}

// TestHostIdentitiesKeepTheOwnersSchema refuses a read facet whose owner is
// another table than its subject names: only the database the host leaves out
// is taken from the owner.
func TestHostIdentitiesKeepTheOwnersSchema(t *testing.T) {
	c := qt.New(t)
	_, planning := hostAlteration(c, ttlCase(ttlTable(hourly), plainTable, "ts"))
	planning.Tables[0].Current.Features.Facets.Records[0].Subject.Schema = part("other")
	plan, err := backend.New().PlanFeatures(c.Context(), planning)
	c.Assert(err, qt.ErrorMatches, `rpc error: code = InvalidArgument desc = YDB creation facet has a different bound identity`)
	c.Assert(plan, qt.IsNil)
}
