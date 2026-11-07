package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Topic"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/changefeeds"
	native "github.com/stokaro/ptah-ydb-plugin/internal/nativeprotocol"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// changefeedDrop is the plan of one operation that drops changefeed f of
// table app/t.
func changefeedDrop(c *qt.C) *protocol.PlannedFeatures {
	c.Helper()
	feed := &native.ObservedChangefeed{Description: &native.ChangefeedDescription{Name: "f", Mode: Ydb_Table.ChangefeedMode_MODE_UPDATES,
		Format: Ydb_Table.ChangefeedFormat_FORMAT_JSON, State: native.ChangefeedDescription_STATE_ENABLED},
		Topic: &native.DescribeTopicResult{PartitioningSettings: &Ydb_Topic.PartitioningSettings{}}}
	operation, err := schema.CaptureChangefeedOperation("drop", &protocol.Reference{Engine: "ydb", Kind: "table", Schema: "app", Name: "t"},
		[]changefeeds.Change{{Name: "f", Before: feed}})
	c.Assert(err, qt.IsNil)
	return &protocol.PlannedFeatures{Contributions: []*protocol.PlanningContribution{{Owner: schema.ProviderID, Steps: []*protocol.PlanningStep{{
		Id: &protocol.StepIdentity{Owner: schema.ProviderID, Name: "drop"}, Operation: &protocol.PlanningOperation{Role: protocol.NativeOperation_STATEMENT, Value: operation}}}}}}
}

// A changefeed's drop is refused on a target without the changefeeds
// capability before anything is rendered. Ported from the host's
// TestRender_Changefeed_RefusesByCapability, whose drop row the built-in
// engine refused by the same key.
func TestRenderRefusesAChangefeedDropWithoutChangefeeds_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := testplanning.RenderRequest(backend.Description(), changefeedDrop(c), map[string]bool{"transactional_ddl": false})

	rendered, err := backend.New().Render(c.Context(), request)

	c.Assert(err, qt.ErrorMatches, `rpc error: code = FailedPrecondition desc = changefeed rendering requires the changefeeds capability`)
	c.Assert(rendered, qt.IsNil)
}

// The control: the same drop renders as one native unit on a target with
// changefeeds.
func TestRenderWritesAChangefeedDropWithChangefeeds_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := testplanning.RenderRequest(backend.Description(), changefeedDrop(c), map[string]bool{"transactional_ddl": false, changefeeds.CapabilityChangefeeds: true})

	rendered, err := backend.New().Render(c.Context(), request)

	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(rendered.Units[0].GetNative().GetKind(), qt.Equals, schema.ChangefeedOperation)
}
