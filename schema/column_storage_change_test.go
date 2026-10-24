package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func storageEnvelope(c *qt.C, representation protocol.ValueInput_Representation, payload string) *protocol.Envelope {
	c.Helper()
	model := schema.NativeDesiredModel(schema.ColumnStorageProperties)
	normalize := schema.NormalizeDesired
	if representation == protocol.ValueInput_OBSERVED {
		for _, candidate := range schema.ObservedModels() {
			if candidate.Kind == schema.ColumnStorageProperties {
				model = candidate
			}
		}
		normalize = schema.Normalize
	}
	value, err := normalize(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: schema.ColumnStorageProperties, SchemaVersion: 1, Encoding: model.Encoding,
		DefinitionSha256: model.DefinitionSha256, Owner: tableOwner(), Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return value
}

// optionalStorage is the column storage of one side, nil for an empty payload,
// which stands for a row table.
func optionalStorage(c *qt.C, representation protocol.ValueInput_Representation, payload string) *protocol.Envelope {
	c.Helper()
	if payload == "" {
		return nil
	}
	return storageEnvelope(c, representation, payload)
}

// TestColumnStorageChange_HappyPath captures a change with both operands, and
// with one of them, the other side being a row table.
func TestColumnStorageChange_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name          string
		before, after string
		want          string
	}{
		{"another shard count", `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64"}`, `{"partitions":"4"}`,
			`{"after":{"partitions":"4"},"before":{"dataChannelCount":"64","hashColumns":["id"],"partitions":"3"}}`},
		{"a column table over a row table", "", `{}`, `{"after":{}}`},
		{"a row table over a column table", `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64"}`, "",
			`{"before":{"dataChannelCount":"64","hashColumns":["id"],"partitions":"3"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			before := optionalStorage(c, protocol.ValueInput_OBSERVED, test.before)
			after := optionalStorage(c, protocol.ValueInput_DESIRED, test.after)
			change, err := schema.CaptureColumnStorageChange("change", tableOwner(), before, after)
			c.Assert(err, qt.IsNil)
			c.Assert(string(change.Value.Payload), qt.Equals, test.want)
			c.Assert(change.ChangeSemantics.Impact, qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			again, err := schema.NormalizeColumnStorageChange(&protocol.ValueInput{Id: "change", Representation: protocol.ValueInput_CHANGE, Value: change.Value})
			c.Assert(err, qt.IsNil)
			c.Assert(string(again.CanonicalProjection), qt.Equals, test.want)
		})
	}
}

func TestColumnStorageChange_FailurePath(t *testing.T) {
	model := schema.ColumnStorageChangeModel()
	for _, test := range []struct{ name, payload string }{
		{"no operand", `{}`},
		{"a null operand", `{"after":null}`},
		{"an unknown key", `{"after":{},"facet":"x"}`},
		{"an invalid observed operand", `{"before":{"partitions":"4294967296"}}`},
		{"an invalid desired operand", `{"after":{"hashColumns":["a","a"]}}`},
		{"not an object", `[]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := qt.New(t)
			change, err := schema.NormalizeColumnStorageChange(&protocol.ValueInput{Id: "change", Representation: protocol.ValueInput_CHANGE, Value: &protocol.Envelope{
				ProviderId: schema.ProviderID, Kind: model.Kind, Owner: tableOwner(), SchemaVersion: 1, Encoding: model.Encoding, DefinitionSha256: model.DefinitionSha256,
				Payload: []byte(test.payload)}})
			c.Assert(err, qt.IsNotNil)
			c.Assert(change, qt.IsNil)
		})
	}
	c := qt.New(t)
	change, err := schema.CaptureColumnStorageChange("change", tableOwner(), nil, nil)
	c.Assert(err, qt.ErrorMatches, `column storage change requires an operand`)
	c.Assert(change, qt.IsNil)
}

// TestColumnStorageConversionLeavesTheExpirationOut converts an observation
// whose TTL carries the expiration the server derives into a declaration that
// states the tiers alone.
func TestColumnStorageConversionLeavesTheExpirationOut(t *testing.T) {
	c := qt.New(t)
	observed := storageEnvelope(c, protocol.ValueInput_OBSERVED, `{"hashColumns":["id"],"partitions":"3","dataChannelCount":"64",`+
		`"ttl":{"enabled":{"column":"ts","expireAfterSeconds":"4294967295","tiers":[{"applyAfterSeconds":"86400","externalStorage":"/local/ext"}]}}}`)
	desired, err := schema.ConvertNative(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)
	c.Assert(err, qt.IsNil)
	c.Assert(string(desired.Payload), qt.Equals, `{"dataChannelCount":"64","hashColumns":["id"],"partitions":"3",`+
		`"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","externalStorage":"/local/ext"}]}}}`)
	back, err := schema.ConvertNative(desired, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	c.Assert(string(back.Payload), qt.Equals, `{"dataChannelCount":"64","hashColumns":["id"],"partitions":"3",`+
		`"ttl":{"enabled":{"column":"ts","tiers":[{"applyAfterSeconds":"86400","externalStorage":"/local/ext"}]}}}`)
}
