package inspect

import (
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// subtreeRegions replaces the exact directory regions of an unscoped read
// with one complete region over the root and every directory below it (ADR
// 0064). A native object declared in a directory that does not exist yet is
// then absent rather than unknown. The directories the walk does not list, a
// dot-prefixed one and the dev realm directory, are claimed as holding
// nothing, as the host's embedded engine read them. Streaming queries keep
// their coverage: the streaming inventory that follows decides it. Views the
// common wire carries are not read as native objects and have no region.
func (c Common) subtreeRegions(result *protocol.Schema, database string) {
	region := func(directory string, knowledge *protocol.ModelKnowledge) *protocol.ModelNamespaceKnowledge {
		return &protocol.ModelNamespaceKnowledge{Knowledge: knowledge, Scope: &protocol.ModelNamespace{
			Database: &protocol.NamespaceComponent{Source: database, Normalized: database}, Catalog: &protocol.NamespaceComponent{},
			Schema: &protocol.NamespaceComponent{Source: directory, Normalized: directory}, SchemaSubtree: true,
		}}
	}
	for _, record := range result.Coverage.Kinds {
		kind := record.Model.Kind
		if !schema.IsNativeObject(kind) || kind == schema.StreamingQueryProperties || (kind == schema.ViewProperties && c.Views) {
			continue
		}
		record.Regions = []*protocol.ModelNamespaceKnowledge{region("", &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE})}
	}
}
