package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// secretOwner is the secret app/ext/pg.pass-1 of /local.
func secretOwner() *protocol.Reference {
	return &protocol.Reference{Engine: "ydb", Kind: schema.SecretProperties, Database: "/local", Schema: "app/ext", Name: "pg.pass-1"}
}

// secretCreation is the operation that creates secretOwner with the value
// PTAH_SECRET_PG holds.
func secretCreation(c *qt.C) *protocol.Envelope {
	c.Helper()
	declared, err := schema.CaptureSecretIntent(secretOwner(), &native.SecretIntent{ValueEnv: "PTAH_SECRET_PG"})
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureSecretChange("change", secretOwner(), nil, declared)
	c.Assert(err, qt.IsNil)
	operation, err := schema.SecretOperationFromChange("operation", change.Value)
	c.Assert(err, qt.IsNil)
	return operation.Value
}

// A read secret converts to a declaration that names the default variable
// for its path, and a declaration converts to the empty observation a read
// lists, its variable left behind. A declared variable is the one the
// secret's statement reads, not the default. Ported from the host's
// TestConversion_NamesTheDefaultVariable and
// TestSecretConversion_CarriesNoVariableIntoAnObservation.
func TestSecretConversion_NamesTheDefaultVariable(t *testing.T) {
	c := testcheck.New(t)
	observed, err := schema.CaptureObject(secretOwner(), &native.SecretPresence{})
	c.Assert(err, qt.IsNil)

	desired, err := schema.ConvertNative(observed, protocol.ValueInput_OBSERVED, protocol.ValueInput_DESIRED)

	c.Assert(err, qt.IsNil)
	read, err := schema.DesiredSecret(desired)
	c.Assert(err, qt.IsNil)
	c.Assert(read.GetValueEnv(), qt.Equals, "PTAH_SECRET_APP_EXT_PG_PASS_1")

	declared, err := schema.CaptureSecretIntent(secretOwner(), &native.SecretIntent{ValueEnv: "PTAH_SECRET_PG"})
	c.Assert(err, qt.IsNil)
	back, err := schema.ConvertNative(declared, protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED)
	c.Assert(err, qt.IsNil)
	c.Assert(string(back.Payload), qt.Equals, `{}`)
	statement, err := schema.SecretStatement(secretCreation(c))
	c.Assert(err, qt.IsNil)
	c.Assert(statement, qt.Equals, "CREATE SECRET `app/ext/pg.pass-1` WITH (value = $PTAH_SECRET_PG);")
}

// Neither model of a secret takes a payload that could carry its value: an
// unknown field, a value among them, a null, a variable a value may not come
// from, or a rotation, which is a request of one comparison and no
// representation carries. An observation carries no field at all. Ported
// from the host's TestModelCodecs_RefuseAWireThatCouldCarryAValue.
func TestSecretModels_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name           string
		representation protocol.ValueInput_Representation
		payload        string
		wantErr        string
	}{
		{"desired value", protocol.ValueInput_DESIRED, `{"value_env":"PTAH_SECRET_PW","value":"s3cr3t"}`, `invalid desired native YDB JSON`},
		{"desired null variable", protocol.ValueInput_DESIRED, `{"value_env":null}`, `null YDB model setting`},
		{"desired variable outside the prefix", protocol.ValueInput_DESIRED, `{"value_env":"HOME"}`, `invalid value_env: "HOME" does not start with PTAH_SECRET_ .*`},
		{"desired null", protocol.ValueInput_DESIRED, `null`, `desired native YDB value requires an object`},
		{"desired rotation", protocol.ValueInput_DESIRED, `{"value_env":"PTAH_SECRET_PW","rotate":true}`, `invalid desired native YDB JSON`},
		{"observed value", protocol.ValueInput_OBSERVED, `{"value":"s3cr3t"}`, `invalid YDB observation JSON`},
		{"observed variable", protocol.ValueInput_OBSERVED, `{"value_env":"PTAH_SECRET_PW"}`, `invalid YDB observation JSON`},
		{"observed rotation", protocol.ValueInput_OBSERVED, `{"rotate":true}`, `invalid YDB observation JSON`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := normalizeIn(test.representation)(forgedValue(schema.SecretProperties, test.representation, secretOwner(), test.payload))
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(value, qt.IsNil)
		})
	}
}

// A secret change takes exactly its two operands, at least one present, and
// neither may carry a value or a rotation flag: a rotation is a change with
// both operands. Ported from the host's TestSecretChange_FailurePath.
func TestSecretChange_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	valid := schema.SecretOperationChange(secretCreation(c))
	for _, test := range []struct {
		name, payload, wantErr string
	}{
		{"null", `null`, `secret change requires exact before and after operands`},
		{"no operands", `{}`, `secret change requires exact before and after operands`},
		{"two absent operands", `{"before":null,"after":null}`, `secret change has no operands`},
		{"an omitted operand", `{"after":{"valueEnv":"PTAH_SECRET_PW"}}`, `secret change requires exact before and after operands`},
		{"a rotation flag", `{"before":{},"after":{"value_env":"PTAH_SECRET_PW","rotate":true}}`, `invalid desired native YDB JSON`},
		{"a value read back", `{"before":{"value":"s3cr3t"},"after":null}`, `invalid YDB observation JSON`},
		{"a declared value", `{"before":null,"after":{"value_env":"PTAH_SECRET_PW","value":"s3cr3t"}}`, `invalid desired native YDB JSON`},
		{"a variable outside the prefix", `{"before":null,"after":{"value_env":"HOME"}}`, `invalid value_env: "HOME" does not start with PTAH_SECRET_ .*`},
		{"an extra field", `{"before":null,"after":{"valueEnv":"PTAH_SECRET_PW"},"extra":true}`, `secret change requires exact before and after operands`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			change, err := schema.NormalizeSecretChange(&protocol.ValueInput{Id: "change", Representation: protocol.ValueInput_CHANGE, Value: payload(valid, test.payload)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(change, qt.IsNil)
		})
	}
}

// A secret operation that names no variable, names its secret by no name or
// by a path, carries a value, names an operation of its own, or drops the
// secret naming a variable writes no statement. Ported from the host's
// TestSecretCodec_FailurePath and TestSecretHandler_FailurePath.
func TestSecretStatement_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	valid := secretCreation(c)
	for _, test := range []struct {
		name, owner, payload, wantErr string
	}{
		{"a missing variable", "pg.pass-1", `{"before":null,"after":{}}`, `invalid value_env: a secret names the environment variable that holds its value`},
		{"no name", "", `{"before":null,"after":{"valueEnv":"PTAH_SECRET_PW"}}`, `invalid YDB object path`},
		{"a path in the name", "ext/pw", `{"before":null,"after":{"valueEnv":"PTAH_SECRET_PW"}}`, `invalid YDB object path`},
		{"a value", "pg.pass-1", `{"before":null,"after":{"valueEnv":"PTAH_SECRET_PW","value":"s3cr3t"}}`, `invalid desired native YDB JSON`},
		{"an operation of its own", "pg.pass-1", `{"before":null,"after":{"valueEnv":"PTAH_SECRET_PW"},"operation":"replace"}`,
			`secret change requires exact before and after operands`},
		{"a drop naming a variable", "pg.pass-1", `{"before":{"valueEnv":"PTAH_SECRET_PW"},"after":null}`, `invalid YDB observation JSON`},
		{"a variable outside the prefix", "pg.pass-1", `{"before":null,"after":{"valueEnv":"HOME"}}`, `invalid value_env: "HOME" does not start with PTAH_SECRET_ .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value := payload(valid, test.payload)
			value.Owner.Name = test.owner
			statement, err := schema.SecretStatement(value)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(statement, qt.Equals, "")
		})
	}
}
