package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// streamingOwner is the streaming query app/copy.
func streamingOwner() *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "app", Name: "copy"}
}

// copyBody is a streaming query body that reads app/source into app/sink.
const copyBody = "INSERT INTO `app/sink` SELECT * FROM `app/source`;"

// A streaming query's model refuses a payload that would decode to another
// query, in either representation: no object, no text, a key it does not
// hold or one in another case, one key twice and a schema statement in the
// body. A declaration refuses a null too. Ported from the host's
// TestModelCodecsRefuseAmbiguousWire.
func TestStreamingModels_RefuseAForgedPayload(t *testing.T) {
	desired, observed := protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED
	for _, test := range []struct {
		name           string
		representation protocol.ValueInput_Representation
		payload        string
		wantErr        string
	}{
		{"a declared null", desired, `null`, `desired native YDB value requires an object`},
		{"an observed null", observed, `null`, `invalid YDB observation JSON`},
		{"a declaration without text", desired, `{}`, `streaming query text is required`},
		{"an observation without text", observed, `{}`, `streaming query text is required`},
		{"a declared extra key", desired, `{"text":"SELECT 1;","extra":true}`, `invalid desired native YDB JSON`},
		{"an observed extra key", observed, `{"text":"SELECT 1;","unknown":false}`, `invalid YDB observation JSON`},
		{"a declared key in another case", desired, `{"Text":"SELECT 1;"}`, `invalid desired native YDB JSON`},
		{"an observed key in another case", observed, `{"Text":"SELECT 1;"}`, `invalid YDB observation JSON`},
		{"a declared key twice", desired, `{"text":"SELECT 1;","text":"SELECT 2;"}`, `invalid model JSON: invalid JSON: duplicate object key "text"`},
		{"an observed key twice", observed, `{"text":"SELECT 1;","text":"SELECT 2;"}`, `invalid YDB observation JSON`},
		{"a declared schema statement", desired, `{"text":"DROP TABLE events;"}`, `streaming query text must contain data statements only`},
		{"an observed schema statement", observed, `{"text":"DROP TABLE events;"}`, `streaming query text must contain data statements only`},
		{"a declared null run", desired, `{"text":"SELECT 1;","run":null}`, `null YDB model setting`},
		{"a declared null permission", desired, `{"text":"SELECT 1;","allowStateReset":null}`, `null YDB model setting`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := normalizeIn(test.representation)(forgedValue(schema.StreamingQueryProperties, test.representation, streamingOwner(), test.payload))
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(value, qt.IsNil)
		})
	}
}

// A read cannot grant the permission to reset a query's state: an observation
// that carries it is refused, and a declaration that leaves it out grants
// none, so a change of the query's text it declares is refused. Ported from
// the host's TestObservedWireCannotGrantResetPermission.
func TestStreamingModels_ObservationGrantsNoStateReset(t *testing.T) {
	c := testcheck.New(t)
	refused, err := schema.Normalize(forgedValue(schema.StreamingQueryProperties, protocol.ValueInput_OBSERVED, streamingOwner(), `{"text":"SELECT 1;","allowStateReset":true}`))
	c.Assert(err, qt.ErrorMatches, `invalid YDB observation JSON`)
	c.Assert(refused, qt.IsNil)

	read, err := schema.Normalize(forgedValue(schema.StreamingQueryProperties, protocol.ValueInput_OBSERVED, streamingOwner(), `{"text":"SELECT 1;"}`))
	c.Assert(err, qt.IsNil)
	declared, err := schema.NormalizeDesired(forgedValue(schema.StreamingQueryProperties, protocol.ValueInput_DESIRED, streamingOwner(), `{"text":"SELECT 2;"}`))
	c.Assert(err, qt.IsNil)
	_, allowed, err := schema.DesiredStreamingQuery(declared)
	c.Assert(err, qt.IsNil)
	c.Assert(allowed, qt.IsFalse)
	change, err := schema.CaptureStreamingQueryChange("change", streamingOwner(), read, declared)
	c.Assert(err, qt.ErrorMatches, `streaming query "app/copy": changing text resets aggregation state; declare allow_state_reset=true to permit it`)
	c.Assert(change, qt.IsNil)
}

// A streaming query operation refuses an owner with no name, a change with
// no operand, a null run or permission in its declaration, a key it does not
// take, an operation key, which no streaming operation names, and one key
// twice. Ported from the host's TestStreamingCodecRefusesIncompleteOrAmbiguousWire.
func TestStreamingQueryOperation_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	declared, err := schema.CaptureStreamingQueryIntent(streamingOwner(), streaming.Spec{Text: copyBody}, false)
	c.Assert(err, qt.IsNil)
	after := string(declared.Payload)
	for _, test := range []struct {
		name, owner, payload, wantErr string
	}{
		{"no name", "", `{"before":null,"after":` + after + `}`, `invalid YDB observation owner`},
		{"no operand", "copy", `{"before":null,"after":null}`, `streaming query change has no operands`},
		{"a null run", "copy", `{"before":null,"after":{"run":null,"text":"SELECT 1;"}}`, `null YDB model setting`},
		{"a null permission", "copy", `{"before":null,"after":{"allowStateReset":null,"text":"SELECT 1;"}}`, `null YDB model setting`},
		{"an unknown key", "copy", `{"before":null,"after":` + after + `,"creation":{"orReplace":false}}`, `streaming query change requires exact before and after operands`},
		{"an operation key", "copy", `{"before":null,"after":` + after + `,"operation":"restart"}`, `streaming query change requires exact before and after operands`},
		{"a key twice", "copy", `{"before":null,"after":` + after + `,"after":` + after + `}`, `invalid model JSON: invalid JSON: duplicate object key "after"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			owner := streamingOwner()
			owner.Name = test.owner
			operation, err := schema.NormalizeStreamingQueryOperation(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION,
				Value: forgedChange(schema.StreamingQueryOperationModel(), owner, test.payload)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(operation, qt.IsNil)
		})
	}
}
