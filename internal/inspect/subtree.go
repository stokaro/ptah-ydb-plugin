package inspect

import (
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// skippedReason is the knowledge of a directory an unscoped read leaves out.
const skippedReason = "The read leaves out dot-prefixed directories and the dev realm directory, and everything below them"

// subtreeRegions replaces the exact directory regions of an unscoped read
// with one complete region over the root and every directory below it, and
// an unknown region over each directory the walk left out with everything
// below it (ADR 0064). A native object declared in a directory that does not
// exist yet is then absent rather than unknown. Streaming queries keep their
// coverage: the streaming inventory that follows decides it. Views the common
// wire carries are not read as native objects and have no region.
func subtreeRegions(result *protocol.Schema, database string, skipped []string, commonViews bool) {
	slices.Sort(skipped)
	skipped = slices.Compact(skipped)
	region := func(directory string, knowledge *protocol.ModelKnowledge) *protocol.ModelNamespaceKnowledge {
		return &protocol.ModelNamespaceKnowledge{Knowledge: knowledge, Scope: &protocol.ModelNamespace{
			Database: &protocol.NamespaceComponent{Source: database, Normalized: database}, Catalog: &protocol.NamespaceComponent{},
			Schema: &protocol.NamespaceComponent{Source: directory, Normalized: directory}, SchemaSubtree: true,
		}}
	}
	for _, record := range result.Coverage.Kinds {
		kind := record.Model.Kind
		if !schema.IsNativeObject(kind) || kind == schema.StreamingQueryProperties || (kind == schema.ViewProperties && commonViews) {
			continue
		}
		record.Regions = []*protocol.ModelNamespaceKnowledge{region("", &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE})}
		for _, directory := range skipped {
			record.Regions = append(record.Regions, region(directory, &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: skippedReason}))
		}
	}
}
