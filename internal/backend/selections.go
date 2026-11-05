package backend

import (
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/schema"
)

// scopeSelections names standalone models with the same directory and
// database-wide rules as their captured identities. Excluding an object also
// excludes its grants. Classifiers and streaming queries keep their required
// pools unless excluded.
//
// Every object that holds an access list of its own has a grant object type
// here. The host reads a grant on that type as a grant on the object: it keeps
// the grant in a scope that keeps the object, and gives the grant's step a
// read of the object, which lets the object's planner put a GRANT after the
// creation and fold a REVOKE into the drop. Without a selection the host keeps
// such a grant in a scope only beside a table of the same name, so a scoped
// plan leaves the declared grant out on both sides and reports nothing to
// change.
func scopeSelections() []*protocol.ScopeSelection {
	return []*protocol.ScopeSelection{
		{Kind: schema.SecretProperties, SelectorType: "secret", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "SECRET"},
		{Kind: schema.StreamingQueryProperties, SelectorType: "streaming_query", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "STREAMING QUERY", KeepsRequired: []string{schema.ResourcePoolProperties}},
		{Kind: schema.CoordinationProperties, SelectorType: "coordination_node", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "COORDINATION NODE"},
		{Kind: schema.TopicProperties, SelectorType: "topic", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "TOPIC"},
		{Kind: schema.ExternalSourceProperties, SelectorType: "external_data_source", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "EXTERNAL DATA SOURCE", KeepsRequired: []string{schema.SecretProperties}},
		{Kind: schema.ExternalTableProperties, SelectorType: "external_table", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "EXTERNAL TABLE", KeepsRequired: []string{schema.ExternalSourceProperties}},
		{Kind: schema.ReplicationProperties, SelectorType: "async_replication", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "ASYNC REPLICATION"},
		{Kind: schema.TransferProperties, SelectorType: "transfer", Naming: protocol.ScopeSelection_IN_SCHEMA, GrantObjectType: "TRANSFER"},
		{Kind: schema.ResourcePoolProperties, SelectorType: "resource_pool", Naming: protocol.ScopeSelection_DATABASE_WIDE, GrantObjectType: "RESOURCE POOL"},
		{Kind: schema.ResourcePoolClassifierProperties, SelectorType: "resource_pool_classifier", Naming: protocol.ScopeSelection_DATABASE_WIDE, KeepsRequired: []string{schema.ResourcePoolProperties}},
	}
}
