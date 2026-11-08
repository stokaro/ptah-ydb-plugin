package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// The payloads of an async replication and a transfer the tests below forge.
const (
	mirrorConnection = `"connection":{"connectionString":"grpc://h:2136/?database=/prod"}`
	mirrorItems      = `"items":[{"source":"a","target":"ra"}]`
	mirror           = `{` + mirrorConnection + `,` + mirrorItems + `}`
	ingestLambda     = `"lambda":"($m) -> { return []; }"`
	ingest           = `{"source":"t","target":"l",` + ingestLambda + `}`
)

// normalizeReplicationChange normalizes a change of either model of the
// replication family by its kind.
func normalizeReplicationChange(kind string) func(*protocol.ValueInput) (*protocol.NormalizedValue, error) {
	return map[string]func(*protocol.ValueInput) (*protocol.NormalizedValue, error){
		schema.ReplicationChange: schema.NormalizeReplicationChange, schema.TransferChange: schema.NormalizeTransferChange,
		schema.ReplicationOperation: schema.NormalizeReplicationOperation, schema.TransferOperation: schema.NormalizeTransferOperation,
	}[kind]
}

// A replication or a transfer change takes exactly its two operands, at least
// one present, each a value of its own kind's model, and two operands that
// are one object as YDB keeps it hold no change: a default spelled out, or a
// lambda with space around it. Ported from the host's
// TestReplicationChange_FailurePath.
func TestReplicationChange_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name       string
		definition *protocol.ValueModel
		payload    string
		wantErr    string
	}{
		{"null", schema.ReplicationChangeModel(), `null`, `async replication change requires exact before and after operands`},
		{"no operands", schema.ReplicationChangeModel(), `{"before":null,"after":null}`, `async replication change has no operands`},
		{"an omitted operand", schema.ReplicationChangeModel(), `{"after":` + mirror + `}`, `async replication change requires exact before and after operands`},
		{"a state on a declaration", schema.ReplicationChangeModel(), `{"before":null,"after":{` + mirrorConnection + `,` + mirrorItems + `,"state":"running"}}`,
			`invalid desired native YDB JSON`},
		{"an extra field", schema.ReplicationChangeModel(), `{"before":null,"after":` + mirror + `,"cascade":true}`, `async replication change requires exact before and after operands`},
		{"a default spelled out", schema.ReplicationChangeModel(), `{"before":` + mirror + `,"after":{` + mirrorConnection + `,` + mirrorItems + `,"consistencyLevel":"row"}}`,
			`async replication operands contain no change`},
		{"a transfer operand in a replication change", schema.ReplicationChangeModel(), `{"before":null,"after":` + ingest + `}`, `invalid desired native YDB JSON`},
		{"the same transfer", schema.TransferChangeModel(), `{"before":` + ingest + `,"after":{"source":"t","target":"l","lambda":" ($m) -> { return []; } "}}`,
			`transfer operands contain no change`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			owner := objectOwner(map[string]string{schema.ReplicationChange: schema.ReplicationProperties, schema.TransferChange: schema.TransferProperties}[test.definition.Kind])
			change, err := normalizeReplicationChange(test.definition.Kind)(&protocol.ValueInput{Id: "change", Representation: protocol.ValueInput_CHANGE,
				Value: forgedChange(test.definition, owner, test.payload)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(change, qt.IsNil)
		})
	}
}

// A replication or a transfer operation refuses a payload with no change, an
// owner with no name or a slash in its name, which its statement would write
// as another object, and operands of the other kind. Ported from the host's
// TestReplicationCodecs_FailurePath. A replication operation also names the
// way it runs, and only a creation waits for its replica tables.
func TestReplicationOperation_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name       string
		definition *protocol.ValueModel
		kind       string
		owner      string
		payload    string
		wantErr    string
	}{
		{"no change", schema.ReplicationOperationModel(), schema.ReplicationProperties, "mirror", `{"operation":"run"}`, `async replication change requires exact before and after operands`},
		{"no name", schema.ReplicationOperationModel(), schema.ReplicationProperties, "", `{"operation":"run","before":` + mirror + `,"after":null}`, `invalid YDB observation owner`},
		{"a slash in the name", schema.TransferOperationModel(), schema.TransferProperties, "a/b", `{"before":` + ingest + `,"after":null}`, `invalid YDB object path`},
		{"transfer operands", schema.ReplicationOperationModel(), schema.ReplicationProperties, "mirror", `{"operation":"run","before":` + ingest + `,"after":null}`, `invalid YDB observation JSON`},
		{"no way to run", schema.ReplicationOperationModel(), schema.ReplicationProperties, "mirror", `{"before":` + mirror + `,"after":null}`,
			`async replication operation names none of \[run create-and-await-replicas\]`},
		{"an await of a drop", schema.ReplicationOperationModel(), schema.ReplicationProperties, "mirror", `{"operation":"create-and-await-replicas","before":` + mirror + `,"after":null}`,
			`async replication operation "create-and-await-replicas" cannot carry out its change`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			owner := objectOwner(test.kind)
			owner.Name = test.owner
			operation, err := normalizeReplicationChange(test.definition.Kind)(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION,
				Value: forgedChange(test.definition, owner, test.payload)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(operation, qt.IsNil)
		})
	}
}

// A replication's or a transfer's model refuses a payload that would decode
// to another object: a key it does not hold at any depth, a key in another
// case, a null, a state on a declaration, a declaration that reads no other
// database or replicates nothing, an observed state, consistency level or
// commit interval YDB does not report, text that holds a NUL or is not
// UTF-8, an observed item without a target, a declared lambda that ends the
// statement, and a declared credential with no connection string. Ported
// from the host's TestModelCodecs_RefuseALossyWire and
// TestModelValidate_FailurePath.
func TestReplicationModels_RefuseAForgedPayload(t *testing.T) {
	desired, observed := protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED
	replication, transfer := schema.ReplicationProperties, schema.TransferProperties
	for _, test := range []struct {
		name           string
		kind           string
		representation protocol.ValueInput_Representation
		payload        string
		wantErr        string
	}{
		{"a declared state", replication, desired, `{` + mirrorConnection + `,` + mirrorItems + `,"state":"running"}`, `invalid desired native YDB JSON`},
		{"an unknown key", replication, desired, `{` + mirrorConnection + `,` + mirrorItems + `,"mode":"x"}`, `invalid desired native YDB JSON`},
		{"an unknown connection key", replication, observed, `{"connection":{"connectionString":"grpc://h:2136/?database=/prod","password":"p"},` + mirrorItems + `}`,
			`invalid YDB observation JSON`},
		{"an unknown item key", replication, observed, `{` + mirrorConnection + `,"items":[{"source":"a","target":"ra","mode":"x"}]}`, `invalid YDB observation JSON`},
		{"a key in another case", replication, desired, `{"Connection":{"connectionString":"grpc://h:2136/?database=/prod"},` + mirrorItems + `}`, `invalid desired native YDB JSON`},
		{"an item key in another case", replication, observed, `{` + mirrorConnection + `,"items":[{"Source":"a","target":"ra"}]}`, `invalid YDB observation JSON`},
		{"a null", replication, desired, `{` + mirrorConnection + `,` + mirrorItems + `,"commitInterval":null}`, `null YDB model setting`},
		{"a declaration with an empty item list", replication, desired, `{` + mirrorConnection + `,"items":[]}`, `it replicates no table; .*`},
		{"a declaration without items", replication, desired, `{` + mirrorConnection + `}`, `it replicates no table; .*`},
		{"a declaration without a connection", replication, desired, `{` + mirrorItems + `}`, `invalid connection_string: a replication reads another database, .*`},
		{"an unknown state", replication, observed, `{` + mirrorConnection + `,` + mirrorItems + `,"state":"stopped"}`, `unknown replication state "stopped"`},
		{"a commit interval at the row level", replication, observed, `{` + mirrorConnection + `,` + mirrorItems + `,"commitInterval":"PT1S"}`,
			`an observed commit interval belongs to the global consistency level`},
		{"an unknown observed consistency level", replication, observed, `{` + mirrorConnection + `,` + mirrorItems + `,"consistencyLevel":"eventual"}`,
			`an observed consistency level is row or global, not "eventual"`},
		{"a NUL in an item", replication, observed, `{` + mirrorConnection + `,"items":[{"source":"a\u0000b","target":"ra"}]}`, `an item's source must be valid UTF-8 without NUL`},
		{"text that is not UTF-8", replication, observed, "{" + mirrorConnection + ",\"items\":[{\"source\":\"\xff\",\"target\":\"ra\"}]}", `invalid YDB observation JSON`},
		{"an observed item without a target", replication, observed, `{` + mirrorConnection + `,"items":[{"source":"a","target":""}]}`,
			`an observed item names a source and a target`},
		{"an unknown transfer connection key", transfer, observed, `{"connection":{"endpoint":"h:2136"},"source":"t","target":"l",` + ingestLambda + `}`, `invalid YDB observation JSON`},
		{"a declared lambda ending the statement", transfer, desired, `{"source":"t","target":"l","lambda":"($m) -> { return []; };"}`, `invalid using .*: ends with a semicolon, .*`},
		{"a declared credential and no connection string", transfer, desired, `{"connection":{"tokenSecretName":"token"},"source":"t","target":"l",` + ingestLambda + `}`,
			`invalid connection_string: a credential signs in to another database, .*`},
		{"an observed fractional flush interval", transfer, observed, `{"source":"t","target":"l",` + ingestLambda + `,"flushInterval":"PT1.5S"}`, `observed flush interval "PT1.5S" .*`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := normalizeIn(test.representation)(forgedValue(test.kind, test.representation, objectOwner(test.kind), test.payload))
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(value, qt.IsNil)
		})
	}
}
