package sourceencode_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/sourceencode"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// HCL has no block for a secret, an external data source or an external
// table, so each declared one is a loss that names the object, and a
// secret's loss names no variable of its value. Ported from the host's
// TestRender_ReportsTheSecretsItLeavesOut, TestRender_ReportsTheSecretsItCannotRecord
// and TestRender_ReportsTheExternalObjectsItLeavesOut.
func TestHCLLosesWhatHasNoBlock(t *testing.T) {
	c := testcheck.New(t)
	for _, test := range []struct {
		name   string
		value  *protocol.NormalizedValue
		encode func(protocol.SourceDocument_Format, *protocol.NormalizedValue) (sourceencode.Result, error)
		want   string
	}{
		{name: "a secret", value: normalized(c, schema.SecretProperties, &native.SecretIntent{ValueEnv: "PTAH_SECRET_PW"}), encode: sourceencode.Secret,
			want: "secret app/eu/a.b: HCL has no block that declares it"},
		{name: "an external data source", value: normalized(c, schema.ExternalSourceProperties, sourceIntent(c)), encode: sourceencode.ExternalSource,
			want: "external data source app/eu/a.b: HCL has no block that declares it"},
		{name: "an external table", value: normalized(c, schema.ExternalTableProperties, tableIntent(c)), encode: sourceencode.ExternalTable,
			want: "external table app/eu/a.b: HCL has no block that declares it"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := test.encode(protocol.SourceDocument_HCL, test.value)
			c.Assert(err, qt.IsNil)
			c.Assert(result.Loss, qt.Equals, test.want)
			c.Assert(result.Loss, qt.Not(qt.Contains), "PTAH_SECRET_")
			c.Assert(result.Parts, qt.HasLen, 0)
		})
	}
}
