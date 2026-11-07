package schema_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// declaredBucket is the payload of a declared external data source.
const declaredBucket = `{"location":"","properties":{"AUTH_METHOD":"NONE"},"sourceType":"ObjectStorage"}`

// An external object change refuses a change with no operand, one that
// leaves a side out, and an observed side that is a declaration. Ported from
// the host's TestExternalChangeCodecs_FailurePath.
func TestExternalChange_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name       string
		definition *protocol.ValueModel
		kind       string
		payload    string
		wantErr    string
	}{
		{"no operand", schema.ExternalSourceChangeModel(), schema.ExternalSourceProperties, `{"before":null,"after":null}`, `external data source change has no operands`},
		{"a missing side", schema.ExternalTableChangeModel(), schema.ExternalTableProperties, `{"after":null}`, `external table change requires exact before and after operands`},
		{"a declaration on the observed side", schema.ExternalSourceChangeModel(), schema.ExternalSourceProperties,
			`{"before":{"access":{},"location":"","properties":{"AUTH_METHOD":"NONE"},"sourceType":"ObjectStorage"},"after":null}`, `invalid YDB observation JSON`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			change, err := schema.NormalizeExternalChange(&protocol.ValueInput{Id: "change", Representation: protocol.ValueInput_CHANGE,
				Value: forgedChange(test.definition, objectOwner(test.kind), test.payload)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(change, qt.IsNil)
		})
	}
}

// An external data source operation refuses a payload that leaves its
// declaration out, an owner with no name or a path in its name, which its
// statement would write as another data source, and an operation it does not
// name. Ported from the host's TestExternalCodecs_FailurePath.
func TestExternalOperation_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, owner, payload, wantErr string
	}{
		{"a missing declaration", "s3", `{"before":null,"operation":"create"}`, `external data source change requires exact before and after operands`},
		{"no name", "", `{"before":null,"after":` + declaredBucket + `,"operation":"create"}`, `invalid YDB observation owner`},
		{"a path in the name", "ext/s3", `{"before":null,"after":` + declaredBucket + `,"operation":"create"}`, `invalid YDB object path`},
		{"an unknown operation", "s3", `{"before":null,"after":` + declaredBucket + `,"operation":"alter"}`,
			`external data source operation names none of \[create replace recreate drop\]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			owner := objectOwner(schema.ExternalSourceProperties)
			owner.Name = test.owner
			operation, err := schema.NormalizeExternalOperation(&protocol.ValueInput{Id: "operation", Representation: protocol.ValueInput_OPERATION,
				Value: forgedChange(schema.ExternalSourceOperationModel(), owner, test.payload)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(operation, qt.IsNil)
		})
	}
}
