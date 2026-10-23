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
