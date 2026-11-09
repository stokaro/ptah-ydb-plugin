package tablecompare_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// pathPart is a part of an identity, nil for the empty one: a table at the
// database root names no directory.
func pathPart(name string) *protocol.ComparisonPart {
	if name == "" {
		return nil
	}
	return testcomparison.Part(name)
}

// directoryTable is the identity of the table name in directory, which is
// empty for the database root.
func directoryTable(directory, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "table", Database: testcomparison.Part("/local"), Schema: pathPart(directory), Name: testcomparison.Part(name)}
}

// changefeedRecord is the changefeed set of the table name in directory on
// one side.
func changefeedRecord(c *qt.C, representation protocol.ValueInput_Representation, directory, name, payload string) *protocol.ComparisonRecord {
	c.Helper()
	model := schema.NativeDesiredModel(schema.ChangefeedProperties)
	if representation == protocol.ValueInput_OBSERVED {
		model = observedModel(schema.ChangefeedProperties)
	}
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: directory + "/" + name, Representation: representation, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.ChangefeedProperties,
			SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256,
			Owner: &protocol.Reference{Engine: "ydb", Kind: "table", Database: "/local", Schema: directory, Name: name}, Payload: []byte(payload)}},
	}})
	c.Assert(err, qt.IsNil)
	return &protocol.ComparisonRecord{Kind: schema.ChangefeedProperties, Subject: directoryTable(directory, name), Value: values.Values[0]}
}

// A table at the root whose name holds a dot and the table of that name in
// the directory before the dot are two tables, so a changefeed declared on
// one is compared with what that table holds and the change names it alone.
// Ported from the host's TestFeatureComparison_LiteralDotsKeepTheCorrectParent.
func TestCompare_AttachesAChangefeedChangeToItsTable(t *testing.T) {
	const none = `{"changefeeds":[]}`
	for _, test := range []struct {
		name string
		// root and nested are the declared changefeeds of the root table
		// tenant.orders and of the table orders in directory tenant.
		root, nested string
		// directory and table name the table the change is reported for.
		directory, table string
	}{
		{"the root table named with a dot", declaredFeed, none, "", "tenant.orders"},
		{"the table in the directory", none, declaredFeed, "tenant", "orders"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			request := &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: tablecompare.ServiceID, Kinds: tablecompare.Kinds(),
				Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)},
				Desired: &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{}, Records: []*protocol.ComparisonRecord{
					changefeedRecord(c, protocol.ValueInput_DESIRED, "", "tenant.orders", test.root),
					changefeedRecord(c, protocol.ValueInput_DESIRED, "tenant", "orders", test.nested)}},
				Current: &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{}, Records: []*protocol.ComparisonRecord{
					changefeedRecord(c, protocol.ValueInput_OBSERVED, "", "tenant.orders", none),
					changefeedRecord(c, protocol.ValueInput_OBSERVED, "tenant", "orders", none)}},
				Parents: []*protocol.ComparisonParent{{Subject: directoryTable("", "tenant.orders"), Desired: true, Current: true},
					{Subject: directoryTable("tenant", "orders"), Desired: true, Current: true}}}
			result := compare(c, request)
			c.Assert(result.Changes, qt.HasLen, 1)
			c.Assert(result.Changes[0].Kind, qt.Equals, schema.ChangefeedProperties)
			c.Assert(proto.Equal(result.Changes[0].Subject, directoryTable(test.directory, test.table)), qt.IsTrue, qt.Commentf("%v", result.Changes[0].Subject))
			c.Assert(result.Changes[0].Value.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_ADDITIVE)
		})
	}
}
