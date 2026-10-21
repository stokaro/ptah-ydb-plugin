//go:build integration

package integration_test

import (
	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testsource"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"testing"
)

func TestCoordinationSourceOfflineSubprocess(t *testing.T) {
	c := qt.New(t)
	binary, digest := providerBinary(c)
	client := startProvider(c, binary, digest)
	request, err := testsource.Request(`{"coordination_nodes":{"workers":{"schema":"app","self_check_period":"PT2S","session_grace_period":"PT5S","read_consistency_mode":"strict","attach_consistency_mode":"relaxed"}}}`)
	c.Assert(err, qt.IsNil)
	// No session or database URL is supplied anywhere in this process test.
	result, err := client.DecodeSource(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateDecodedSource(client.Description(), request, result), qt.IsNil)
	value := result.Documents[0].Fragments[0].Values[0].Value
	normalized, err := client.NormalizeValues(c.Context(), &protocol.NormalizeValuesRequest{Engine: "ydb", Values: []*protocol.ValueInput{{Id: "roundtrip", Representation: protocol.ValueInput_DESIRED, Value: value.Value}}})
	c.Assert(err, qt.IsNil)
	c.Assert(string(normalized.Values[0].CanonicalProjection), qt.Equals, string(value.CanonicalProjection))
	bad, err := testsource.Request(`{"coordination_nodes":{"good":{},"bad":{"self_check_period":"PT0.1S"}}}`)
	c.Assert(err, qt.IsNil)
	refused, err := client.DecodeSource(c.Context(), bad)
	c.Assert(err, qt.IsNotNil)
	c.Assert(refused, qt.IsNil)
}
