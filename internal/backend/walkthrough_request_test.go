package backend_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/encoding/protojson"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// walkthroughRequest is the request ydb-transfers received in the baseline
// run of Ptah's YDB walkthrough: `ptah schema compare` of the walkthrough's
// schema against an empty directory shop, with Ptah v0.25.0 as the host,
// captured unchanged from the host's call. Ptah's guide shows an excerpt of
// it and links this file as the complete request.
func walkthroughRequest(c *qt.C) *protocol.CompareModelsRequest {
	c.Helper()
	encoded, err := os.ReadFile(filepath.Join("testdata", "walkthrough", "compare-transfers.json"))
	c.Assert(err, qt.IsNil)
	request := &protocol.CompareModelsRequest{}
	c.Assert(protojson.Unmarshal(encoded, request), qt.IsNil)
	return request
}

// The walkthrough's request passes the SDK's request validation and reports
// one change, the new transfer shop/ingest_orders: its target table is among
// the declared relations and its source among the changefeeds orders
// declares.
func TestCompareModelsReadsTheWalkthroughRequest_HappyPath(t *testing.T) {
	c := testcheck.New(t)

	result, err := backend.New().CompareModels(c.Context(), walkthroughRequest(c))

	c.Assert(err, qt.IsNil)
	c.Assert(result.Undecided, qt.HasLen, 0)
	c.Assert(result.Changes, qt.HasLen, 1)
	c.Assert(result.Changes[0].Kind, qt.Equals, schema.TransferProperties)
	c.Assert(result.Changes[0].Subject.GetSchema().GetNormalized(), qt.Equals, "shop")
	c.Assert(result.Changes[0].Subject.GetName().GetNormalized(), qt.Equals, "ingest_orders")
}

// The same request without the changefeeds orders declares is refused: the
// read covered shop and found no such stream, and the declaration no longer
// names one, so the transfer would read a topic the plan leaves absent.
func TestCompareModelsReadsTheWalkthroughRequest_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := walkthroughRequest(c)
	request.DeclaredFacets = nil

	result, err := backend.New().CompareModels(c.Context(), request)

	c.Assert(err, qt.ErrorMatches, "rpc error: code = InvalidArgument desc = transfer shop/ingest_orders: it reads topic shop/orders/feed, which the "+
		"schema declares neither as a topic nor as a changefeed, so the plan leaves no such topic, .*")
	c.Assert(result, qt.IsNil)
}
