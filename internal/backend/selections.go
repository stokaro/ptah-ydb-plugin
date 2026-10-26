package backend

import (
	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// scopeSelections names standalone models with the same directory and
// database-wide rules as their captured identities. Excluding an object also
// excludes its grants. Classifiers and streaming queries keep their required
// pools unless excluded.
func scopeSelections() []*protocol.ScopeSelection {
	return []*protocol.ScopeSelection{
		{Kind: schema.SecretProperties, SelectorType: "secret", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "SECRET"},
		{Kind: schema.StreamingQueryProperties, SelectorType: "streaming_query", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "STREAMING QUERY", KeepsRequired: []string{schema.ResourcePoolProperties}},
		{Kind: schema.CoordinationProperties, SelectorType: "coordination_node", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "COORDINATION NODE"},
		{Kind: schema.TopicProperties, SelectorType: "topic", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "TOPIC"},
		{Kind: schema.ExternalSourceProperties, SelectorType: "external_data_source", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "EXTERNAL DATA SOURCE"},
		{Kind: schema.ExternalTableProperties, SelectorType: "external_table", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "EXTERNAL TABLE"},
		{Kind: schema.ResourcePoolProperties, SelectorType: "resource_pool", Naming: protocol.ScopeSelection_DATABASE_WIDE, GrantObjectType: "RESOURCE POOL"},
		{Kind: schema.ResourcePoolClassifierProperties, SelectorType: "resource_pool_classifier", Naming: protocol.ScopeSelection_DATABASE_WIDE, KeepsRequired: []string{schema.ResourcePoolProperties}},
	}
}
