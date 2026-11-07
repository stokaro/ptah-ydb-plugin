package reversal_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/reversal"
	"github.com/stokaro/ptah-ydb-plugin/internal/streaming"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// valueOf is a value of kind under owner in representation, desired or
// observed, normalized from a payload written by hand.
func valueOf(c *qt.C, kind string, representation protocol.ValueInput_Representation, owner *protocol.Reference, payload string) *protocol.Envelope {
	c.Helper()
	definition, normalize := schema.NativeDesiredModel(kind), schema.NormalizeDesired
	for _, observed := range schema.ObservedModels() {
		if representation == protocol.ValueInput_OBSERVED && observed.Kind == kind {
			definition, normalize = observed, schema.Normalize
		}
	}
	value, err := normalize(&protocol.Envelope{ProviderId: schema.ProviderID, Kind: kind, Owner: owner, SchemaVersion: definition.SchemaVersion,
		Encoding: definition.Encoding, DefinitionSha256: definition.DefinitionSha256, Payload: []byte(payload)})
	c.Assert(err, qt.IsNil)
	return value
}

// reversed reverses change alone, each prediction kept as normalization
// returns it.
func reversed(c *qt.C, change *protocol.NormalizedValue) *protocol.ReversedChange {
	c.Helper()
	observe := func(id string, value *protocol.Envelope) (*protocol.NormalizedValue, error) {
		return &protocol.NormalizedValue{Id: id, Value: value}, nil
	}
	// A target without a family's capability refuses to reverse its change,
	// so the request states every capability the changes here need.
	capabilities := map[string]bool{"streaming_queries": true, "topics": true, "async_replication": true, "transfers": true,
		"secrets": true, "external_data_sources": true, "coordination_nodes": true, "resource_pools": true}
	result, err := reversal.Reverse(c.Context(), &protocol.ReverseChangesRequest{Changes: []*protocol.PlanningChange{{Value: change}}, Capabilities: capabilities}, observe)
	c.Assert(err, qt.IsNil)
	c.Assert(result.Changes, qt.HasLen, 1)
	return result.Changes[0]
}

// Restoring how a streaming query runs keeps its checkpoint, so the reverse
// of a change of its settings alone claims no loss of state. Ported from the
// host's TestStreamingSettingReversalDoesNotClaimCheckpointLoss.
func TestReverse_StreamingSettingsKeepTheCheckpoint(t *testing.T) {
	c := testcheck.New(t)
	owner := &protocol.Reference{Engine: "ydb", Kind: schema.StreamingQueryProperties, Schema: "app", Name: "query"}
	read := valueOf(c, schema.StreamingQueryProperties, protocol.ValueInput_OBSERVED, owner, `{"text":"SELECT 1;"}`)
	stopped, err := schema.CaptureStreamingQueryIntent(owner, streaming.Spec{Text: "SELECT 1;", Run: new(false)}, false)
	c.Assert(err, qt.IsNil)
	change, err := schema.CaptureStreamingQueryChange("change", owner, read, stopped)
	c.Assert(err, qt.IsNil)

	reverse := reversed(c, change)

	c.Assert(reverse.Strategy, qt.Equals, "restore streaming execution settings in place")
	c.Assert(reverse.Limitations, qt.HasLen, 0)
}

// The reverse of a changefeed's drop declares it again and says what adding
// it again cannot restore: the records of the dropped stream and its
// consumers' positions. Ported from the host's
// TestRegisteredYDBReversalRestoresDefinitionsAndReportsStateLoss.
func TestReverse_DroppedChangefeedReportsTheLostStream(t *testing.T) {
	c := testcheck.New(t)
	owner := &protocol.Reference{Engine: "ydb", Database: "/local", Schema: "app", Kind: "table", Name: "records"}
	held := valueOf(c, schema.ChangefeedProperties, protocol.ValueInput_OBSERVED, owner,
		`{"changefeeds":[{"description":{"name":"f","mode":"MODE_UPDATES","format":"FORMAT_JSON","state":"STATE_ENABLED"},"topic":{"partitioningSettings":{},"consumers":[{"name":"audit"}]}}]}`)
	none := valueOf(c, schema.ChangefeedProperties, protocol.ValueInput_DESIRED, owner, `{"changefeeds":[]}`)
	change, err := schema.CaptureTablePropertiesChange("change", owner, held, none)
	c.Assert(err, qt.IsNil)

	reverse := reversed(c, change)

	c.Assert(reverse.Strategy, qt.Equals, "restore the changefeeds the table held")
	c.Assert(reverse.Limitations, qt.DeepEquals, []string{
		"a changefeed of table app/records the rollback adds again starts a new stream; the messages and the consumers' positions of the dropped one are lost"})
	c.Assert(string(reverse.Value.Value.Payload), qt.Contains, `"after":{"changefeeds":[{"format":"FORMAT_JSON","mode":"MODE_UPDATES","name":"f","topic":{"consumers":[{"name":"audit"}]`)
}
