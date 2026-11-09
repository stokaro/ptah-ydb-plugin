package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/internal/backend"
	"github.com/stokaro/ptah-ydb-plugin/internal/testcheck"
)

// rewritten is the HCL document an export of request writes, with the text
// of every attribute named key replaced by value: a document a person edited
// after the export.
func rewritten(c *qt.C, ctx context.Context, request *protocol.EncodeSourceRequest, key, value string) *protocol.DecodeSourceRequest {
	c.Helper()
	encoded, err := backend.New().EncodeSource(ctx, request)
	c.Assert(err, qt.IsNil)
	document := sourceDocument(protocol.SourceDocument_HCL, request, encoded)
	replaced := 0
	for _, fragment := range document.Documents[0].Fragments {
		for _, entry := range fragment.GetValue().GetMapping().GetEntries() {
			if entry.GetKey() == key {
				entry.Value = text(value)
				replaced++
			}
		}
	}
	c.Assert(replaced, qt.Equals, 1)
	return document
}

// An HCL index block of a global index carrying a partition count of zero is
// refused, naming the index. Ported from the host's
// TestYDBIndexPartitioning_RejectsInvalidSettings.
func TestHCLIndexBlockRefusesAPartitionCountOfZero(t *testing.T) {
	c := testcheck.New(t)
	request := tableRequest(protocol.SourceDocument_HCL, indexFacet(c, "by_body", `{"partitioningSettings":{"minPartitionsCount":"2","partitioningByLoad":"ENABLED"}}`))
	document := rewritten(c, groupedEncodingContext(c), request, "auto_partitioning_min_partitions_count", "0")

	decoded, err := backend.New().DecodeSource(groupedEncodingContext(c), document)

	c.Assert(err, qt.ErrorMatches, `.*index "by_body": invalid auto_partitioning_min_partitions_count "0": write a whole number of at least 1; YDB refuses a count of zero`)
	c.Assert(decoded, qt.IsNil)
}
