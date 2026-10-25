package tablecreate_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testplanning"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// textRendering is the TEXT render of a planned creation.
func textRendering(c *qt.C, request *protocol.PlanFeaturesRequest) *protocol.RenderRequest {
	c.Helper()
	plan, err := backend.New().PlanFeatures(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(plan.Diagnostics, qt.HasLen, 0)
	rendering := testplanning.RenderRequest(plan, request.Capabilities)
	rendering.Form = protocol.RenderRequest_TEXT
	return rendering
}

// A planned creation renders in the TEXT form as the YQL statement of its
// native call, which the SDK takes as a text unit.
func TestTableCreationRendersText_HappyPath(t *testing.T) {
	c := qt.New(t)
	request := creationPlan(c)
	request.Tables[0].Desired.Features.Facets.Records = []*protocol.ComparisonRecord{
		facet(c, schema.TableProperties, "", `{"primaryKey":["second","first"],"columnFamilies":[{"name":"archive"}],"keyBloomFilter":"DISABLED","partitioningSettings":{"minPartitionsCount":"1","maxPartitionsCount":"2"}}`),
		facet(c, schema.ColumnProperties, "value", `{"family":"archive"}`),
	}
	rendering := textRendering(c, request)
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Refusal, qt.IsNil)
	c.Assert(rendered.Units, qt.HasLen, 1)
	c.Assert(rendered.Units[0].GetSql(), qt.Equals, "CREATE TABLE `app/records` (\n    `first` Uint64 NOT NULL,\n    `second` Utf8 NOT NULL,\n"+
		"    `value` Decimal(22,9) FAMILY `archive`,\n    PRIMARY KEY (`second`, `first`),\n    FAMILY `archive` ()\n) "+
		"WITH (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1, AUTO_PARTITIONING_MAX_PARTITIONS_COUNT = 2, KEY_BLOOM_FILTER = DISABLED);")
	c.Assert(provider.ValidateRenderedBatch(backend.Description(), rendering, rendered), qt.IsNil)
}

// A creation that sets a user attribute, which YQL's CREATE TABLE does not
// set, refuses the whole TEXT batch with NO_TEXT_FORM and no unit; the ANY
// form renders it natively.
func TestTableCreationRendersText_FailurePath(t *testing.T) {
	c := qt.New(t)
	rendering := textRendering(c, creationPlan(c))
	rendered, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.Complete, qt.IsFalse)
	c.Assert(rendered.Units, qt.HasLen, 0)
	c.Assert(rendered.Refusal.ChangeId, qt.Equals, rendering.Changes[0].Id)
	c.Assert(rendered.Refusal.Reason, qt.Equals, protocol.RenderRefusal_NO_TEXT_FORM)
	c.Assert(rendered.Refusal.Message, qt.Equals, "no YQL text form: YQL's CREATE TABLE sets no user attribute")
	c.Assert(provider.ValidateRenderedBatch(backend.Description(), rendering, rendered), qt.IsNil)

	rendering.Form = protocol.RenderRequest_ANY
	native, err := backend.New().Render(c.Context(), rendering)
	c.Assert(err, qt.IsNil)
	c.Assert(native.Units, qt.HasLen, 1)
	c.Assert(native.Units[0].GetNative(), qt.IsNotNil)
}
