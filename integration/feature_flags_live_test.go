//go:build integration

package integration_test

import (
	"context"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// differingKeys are the capability keys whose values differ between two
// sessions, or that one session reports and the other does not, sorted.
func differingKeys(first, second map[string]bool) []string {
	var keys []string
	for key, value := range first {
		if other, found := second[key]; !found || other != value {
			keys = append(keys, key)
		}
	}
	for key := range second {
		if _, found := first[key]; !found {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// Ported from the host's TestYDBConnection_ReadsTheClusterFeatureFlags_HappyPath:
// a session whose address names the monitoring endpoint reads the cluster's
// feature flags there, and one whose address does not reports the release
// line's defaults. The sessions differ exactly in the capabilities the flags
// the test servers start with turn on, as scripts/live-lines.sh starts them:
// enable_external_data_sources on both lines, enable_vector_index on 25.1,
// where vector indexes are off by default, and enable_add_unique_index and
// enable_streaming_queries on 26.2. The host's test also reads the
// capabilities through a scoped session of its own connection, which this
// provider has no counterpart of.
func TestSessionReadsTheClusterFeatureFlags_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name     string
		before26 bool
		flagged  []string
	}{
		{name: "a 25.x line", before26: true, flagged: []string{"external_data_sources", "vector_indexes"}},
		{name: "a 26.x line", flagged: []string{"external_data_sources", "streaming_queries", "unique_index_on_existing_table"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			opened, _, _ := inspectionFixture(c)
			skipOnLine(c, test.before26, serverVersion(c, opened))
			address := monitoredAddress(c)
			client := startProviderFor(c)
			monitored, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: address})
			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), monitored.Id), qt.IsNil) })

			preset, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "ydb", Address: withoutMonitoring(c, address)})

			c.Assert(err, qt.IsNil)
			c.Cleanup(func() { c.Check(client.CloseSession(context.Background(), preset.Id), qt.IsNil) })
			c.Assert(differingKeys(monitored.Capabilities, preset.Capabilities), qt.DeepEquals, test.flagged)
			for _, key := range test.flagged {
				c.Assert(monitored.Capabilities[key], qt.IsTrue, qt.Commentf("%s", key))
			}
		})
	}
}
