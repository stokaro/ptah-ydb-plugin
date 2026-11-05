package tablecompare_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/tablecompare"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcomparison"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

// indexSubject names index name of app.records as the host names an index's
// own facet.
func indexSubject(name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "index", Database: testcomparison.Part("/local"), Schema: testcomparison.Part("app"),
		Parent: testcomparison.Part("records"), Name: testcomparison.Part(name)}
}

// indexRecord is the facet of index owner of app.records in one
// representation, which the host sends under subject.
func indexRecord(c *qt.C, representation protocol.ValueInput_Representation, owner, subject, payload string) *protocol.ComparisonRecord {
	c.Helper()
	model := schema.NativeDesiredModel(schema.IndexSettingsProperties)
	if representation == protocol.ValueInput_OBSERVED {
		for _, candidate := range schema.ObservedModels() {
			if candidate.Kind == schema.IndexSettingsProperties {
				model = candidate
			}
		}
	}
	reference := &protocol.Reference{Engine: "ydb", Kind: "index", Database: "/local", Schema: "app", Path: []string{"records"}, Name: owner}
	values, err := backend.New().NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{
		{Id: "index." + owner, Representation: representation, Value: &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.IndexSettingsProperties,
			SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Owner: reference, Payload: []byte(payload)}},
	}})
	c.Assert(err, qt.IsNil)
	return &protocol.ComparisonRecord{Kind: schema.IndexSettingsProperties, Subject: indexSubject(subject), Value: values.Values[0]}
}

// indexRequest compares the declared facet of index by_label with the one the
// read gave the index held as heldName, which the host sends under by_label.
func indexRequest(c *qt.C, declared, heldName, held string) *protocol.CompareModelsRequest {
	c.Helper()
	wanted := &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Coverage: &protocol.ComparisonCoverage{},
		Records: []*protocol.ComparisonRecord{indexRecord(c, protocol.ValueInput_DESIRED, "by_label", "by_label", declared)}}
	current := &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Coverage: &protocol.ComparisonCoverage{},
		Records: []*protocol.ComparisonRecord{indexRecord(c, protocol.ValueInput_OBSERVED, heldName, "by_label", held)}}
	return &protocol.CompareModelsRequest{Engine: "ydb", ComparisonId: tablecompare.ServiceID, Kinds: tablecompare.Kinds(),
		Identifiers: &protocol.ComparisonIdentifiers{Rules: proto.CloneOf(backend.Description().Engines[0].Identifiers)}, Desired: wanted, Current: current,
		Parents: []*protocol.ComparisonParent{{Subject: indexSubject("by_label"), Desired: true, Current: true}}}
}

const heldIndexDefaults = `{"partitioningSettings":{"minPartitionsCount":"1","partitionSizeMb":"2048","partitioningByLoad":"DISABLED","partitioningBySize":"ENABLED"}}`

// TestCompare_IndexSettings reports an index whose own facet declares a
// setting it does not hold, under the index's subject, and nothing for one
// that holds every declared setting: a setting left out keeps what the index
// holds. A renamed index's read reaches the comparison under the declared
// name and is compared there.
func TestCompare_IndexSettings(t *testing.T) {
	for _, test := range []struct {
		name, declared, heldName, held string
		changes                        int
	}{
		{"another partition count", `{"partitioningSettings":{"minPartitionsCount":"3"}}`, "by_label", heldIndexDefaults, 1},
		{"read replicas the index lacks", `{"readReplicasSettings":{"perAzReadReplicasCount":"1"}}`, "by_label", heldIndexDefaults, 1},
		{"settings the index holds", `{"partitioningSettings":{"partitioningBySize":"ENABLED","minPartitionsCount":"1"}}`, "by_label", heldIndexDefaults, 0},
		{"nothing declared", `{}`, "by_label", heldIndexDefaults, 0},
		{"a renamed index with another partition count", `{"partitioningSettings":{"minPartitionsCount":"3"}}`, "by_old", heldIndexDefaults, 1},
		{"a renamed index that keeps its settings", `{"partitioningSettings":{"minPartitionsCount":"1"}}`, "by_old", heldIndexDefaults, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result := compare(c, indexRequest(c, test.declared, test.heldName, test.held))
			c.Assert(result.Changes, qt.HasLen, test.changes)
			for _, change := range result.Changes {
				c.Assert(change.Kind, qt.Equals, schema.IndexSettingsProperties)
				c.Assert(proto.Equal(change.Subject, indexSubject("by_label")), qt.IsTrue)
				c.Assert(change.Value.Value.Kind, qt.Equals, schema.TablePropertiesChange)
				c.Assert(change.Value.Value.Owner.Name, qt.Equals, "by_label")
				c.Assert(change.Value.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			}
		})
	}
}
