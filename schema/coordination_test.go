package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"
)

func TestCoordinationIntentPreservesOmission(t *testing.T) {
	c := qt.New(t)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Schema: "app", Name: "node"}
	base, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{})
	c.Assert(err, qt.IsNil)
	c.Assert(string(base.Payload), qt.Equals, `{}`)
	mode := "relaxed"
	explicit, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{ReadConsistencyMode: &mode})
	c.Assert(err, qt.IsNil)
	c.Assert(string(explicit.Payload), qt.Equals, `{"read_consistency_mode":"relaxed"}`)
	for _, payload := range []string{`null`, `[]`, `{"unknown":1}`, `{"self_check_period_millis":null}`, `{"self_check_period_millis":0}`, `{"self_check_period_millis":1.5}`, `{"self_check_period_millis":1e3}`, `{"self_check_period_millis":4294967296}`, `{"self_check_period_millis":10000}`, `{"read_consistency_mode":""}`, `{"read_consistency_mode":"STRICT"}`, `{"read_consistency_mode":"strict","read_consistency_mode":"relaxed"}`, `{"struct_name":null}`, `{"self":{"owner":"root"}}`, `{"READ_CONSISTENCY_MODE":"strict"}`, `{"read_consistency_mode":"strict","READ_CONSISTENCY_MODE":"relaxed"}`} {
		input := proto.CloneOf(base)
		input.Payload = []byte(payload)
		result, err := schema.NormalizeDesired(input)
		c.Assert(err, qt.IsNotNil, qt.Commentf("payload %s", payload))
		c.Assert(result, qt.IsNil)
	}
	_, err = schema.Normalize(base)
	c.Assert(err, qt.IsNotNil)
	c.Assert(schema.CoordinationDesiredModel().Representation, qt.Equals, protocol.ValueInput_DESIRED)
	for _, model := range schema.ObservedModels() {
		c.Assert(model.Representation, qt.Equals, protocol.ValueInput_OBSERVED)
	}
}

func TestCoordinationIntentRefusesLossySourceHolder(t *testing.T) {
	c := qt.New(t)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.CoordinationProperties, Name: "node"}
	value, err := schema.CaptureCoordinationIntent(owner, schema.CoordinationIntent{StructName: string([]byte{0xff})})
	c.Assert(err, qt.IsNotNil)
	c.Assert(value, qt.IsNil)
}
