package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

func tableOperand(c *qt.C, representation protocol.ValueInput_Representation, payload string) *protocol.Envelope {
	c.Helper()
	model := schema.TableDesiredModel()
	if representation == protocol.ValueInput_OBSERVED {
		for _, candidate := range schema.ObservedModels() {
			if candidate.Kind == schema.TableProperties {
				model = candidate
			}
		}
	}
	return &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.TableProperties, Owner: tableOwner(), SchemaVersion: 1,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: []byte(payload)}
}

const (
	plainDesired  = `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}]}`
	plainObserved = `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}]}`
	ttlDesired    = `{"primaryKey":["id"],"columnFamilies":[{"name":"default"}],"ttlSettings":{"dateTypeColumn":{"columnName":"ts","expireAfterSeconds":3600}}}`
)

func TestTablePropertiesChange_HappyPath(t *testing.T) {
	c := qt.New(t)
	before := tableOperand(c, protocol.ValueInput_OBSERVED, plainObserved)
	after := tableOperand(c, protocol.ValueInput_DESIRED, ttlDesired)

	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), before, after)

	c.Assert(err, qt.IsNil)
	c.Assert(change.Value.Kind, qt.Equals, schema.TablePropertiesChange)
	c.Assert(change.ChangeSemantics.GetReplacesOwner(), qt.IsFalse)
	c.Assert(change.ChangeSemantics.GetRemovesState(), qt.IsFalse)
	c.Assert(change.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
	c.Assert(change.ChangeSemantics.Access, qt.Equals, protocol.ChangeSemantics_UNCHANGED)
	again, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "change", Representation: protocol.ValueInput_CHANGE, Value: change.Value})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(again, change), qt.IsTrue)
}

func TestTablePropertiesChange_FailurePath(t *testing.T) {
	c := qt.New(t)
	before := tableOperand(c, protocol.ValueInput_OBSERVED, plainObserved)
	after := tableOperand(c, protocol.ValueInput_DESIRED, ttlDesired)
	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), before, after)
	c.Assert(err, qt.IsNil)

	for _, payload := range []string{
		`null`, `[]`, `{}`, `{"before":null}`, `{"before":null,"after":null}`, `{"before":{},"after":null}`, `{"before":null,"after":{}}`,
		`{"Before":{},"after":{}}`, `{"before":{},"after":{},"removes_state":true}`,
		`{"before":{"unknown":1},"after":{}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			c := qt.New(t)
			value := proto.CloneOf(change.Value)
			value.Payload = []byte(payload)

			_, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_CHANGE, Value: value})

			c.Assert(err, qt.IsNotNil)
		})
	}
	t.Run("a missing operand", func(t *testing.T) {
		c := qt.New(t)

		_, err := schema.CaptureTablePropertiesChange("change", tableOwner(), nil, after)

		c.Assert(err, qt.ErrorMatches, "table properties change requires both operands")
	})
	t.Run("operands of another owner", func(t *testing.T) {
		c := qt.New(t)
		other := tableOwner()
		other.Name = "other"

		_, err := schema.CaptureTablePropertiesChange("change", other, before, after)

		c.Assert(err, qt.ErrorMatches, "table properties change operands have different owners")
	})
	t.Run("an owner that is neither a table nor a column", func(t *testing.T) {
		c := qt.New(t)
		value := proto.CloneOf(change.Value)
		value.Owner.Kind = "index"

		_, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_CHANGE, Value: value})

		c.Assert(err, qt.ErrorMatches, "table properties change requires a table or column owner")
	})
	t.Run("a foreign definition", func(t *testing.T) {
		c := qt.New(t)
		value := proto.CloneOf(change.Value)
		value.DefinitionSha256 = "foreign"

		_, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_CHANGE, Value: value})

		c.Assert(err, qt.ErrorMatches, "table properties change does not match its negotiated definition")
	})
}

const (
	noIndexesObserved = `{}`
	noIndexesDesired  = `{}`
	oneIndexDesired   = `{"indexes":[{"specification":{"name":"by_label","indexColumns":["label"],"globalIndex":{}},"implementation":[{"name":"indexImplTable",` +
		`"settings":{"primaryKey":["label","id"]},"columns":[{"name":"label","type":{"typeId":"UTF8"},"settings":{}},{"name":"id","type":{"typeId":"UINT64"},"settings":{}}]}]}]}`
)

func indexOperand(c *qt.C, representation protocol.ValueInput_Representation, payload string) *protocol.Envelope {
	c.Helper()
	model := schema.NativeDesiredModel(schema.IndexProperties)
	if representation == protocol.ValueInput_OBSERVED {
		for _, candidate := range schema.ObservedModels() {
			if candidate.Kind == schema.IndexProperties {
				model = candidate
			}
		}
	}
	return &protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.IndexProperties, Owner: tableOwner(), SchemaVersion: 1,
		Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256, Payload: []byte(payload)}
}

func TestTablePropertiesChange_IndexSetsHappyPath(t *testing.T) {
	c := qt.New(t)
	before := indexOperand(c, protocol.ValueInput_OBSERVED, noIndexesObserved)
	after := indexOperand(c, protocol.ValueInput_DESIRED, oneIndexDesired)

	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), before, after)

	c.Assert(err, qt.IsNil)
	c.Assert(change.Value.Kind, qt.Equals, schema.TablePropertiesChange)
	c.Assert(string(change.Value.Payload), qt.Contains, `"facet":"`+schema.IndexProperties+`"`)
	c.Assert(change.ChangeSemantics.GetReplacesOwner(), qt.IsFalse)
	c.Assert(change.ChangeSemantics.GetRemovesState(), qt.IsFalse)
	c.Assert(change.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
	again, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "change", Representation: protocol.ValueInput_CHANGE, Value: change.Value})
	c.Assert(err, qt.IsNil)
	c.Assert(proto.Equal(again, change), qt.IsTrue)
}

// indexChangePayload states a change payload by hand: empty observed index sets
// before, the declared index after, and the given extra keys.
func indexChangePayload(extra string) string {
	return `{"before":{},"after":` + oneIndexDesired + extra + `}`
}

func TestTablePropertiesChange_IndexSetsFailurePath(t *testing.T) {
	c := qt.New(t)
	before := indexOperand(c, protocol.ValueInput_OBSERVED, noIndexesObserved)
	after := indexOperand(c, protocol.ValueInput_DESIRED, oneIndexDesired)
	change, err := schema.CaptureTablePropertiesChange("change", tableOwner(), before, after)
	c.Assert(err, qt.IsNil)
	table := tableOperand(c, protocol.ValueInput_DESIRED, plainDesired)
	facet := `,"facet":"` + schema.IndexProperties + `"`

	t.Run("operands of different models", func(t *testing.T) {
		c := qt.New(t)

		_, err := schema.CaptureTablePropertiesChange("change", tableOwner(), before, table)

		c.Assert(err, qt.ErrorMatches, "table properties change operands are facets of different models")
	})
	for _, test := range []struct {
		name    string
		payload string
		owner   *protocol.Reference
		message string
	}{
		{"a facet no model owns", indexChangePayload(`,"facet":"other"`), tableOwner(), "table properties change names an unsupported facet"},
		{"a facet on a column owner", indexChangePayload(facet), columnOwner(), "table properties change names an unsupported facet"},
		{"a facet the table properties model owns", indexChangePayload(`,"facet":"` + schema.TableProperties + `"`), tableOwner(), "table properties change names an unsupported facet"},
		{"an extra key", indexChangePayload(facet + `,"more":1`), tableOwner(), "table properties change requires exact before and after operands"},
		{"index sets read as table properties", indexChangePayload(""), tableOwner(), ".*"},
		{"a before operand of another model", `{"before":{"primaryKey":["id"]},"after":` + oneIndexDesired + facet + `}`, tableOwner(), ".*"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			value := proto.CloneOf(change.Value)
			value.Payload, value.Owner = []byte(test.payload), test.owner

			_, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "forged", Representation: protocol.ValueInput_CHANGE, Value: value})

			c.Assert(err, qt.ErrorMatches, test.message)
		})
	}
	// The control: the same hand-written payload with the facet and the table owner is accepted.
	t.Run("the control is accepted", func(t *testing.T) {
		c := qt.New(t)
		value := proto.CloneOf(change.Value)
		value.Payload = []byte(indexChangePayload(facet))

		_, err := schema.NormalizeTablePropertiesChange(&protocol.ValueInput{Id: "control", Representation: protocol.ValueInput_CHANGE, Value: value})

		c.Assert(err, qt.IsNil)
	})
}
