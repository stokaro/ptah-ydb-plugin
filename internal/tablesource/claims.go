// Package tablesource decodes the table-scope declarations a YAML or Go
// annotation source writes for a YDB row table: its automatic partitioning,
// read replicas, key bloom filter and column families.
//
// The host lexes the source and sends the provider the parsed values of the
// keys and directives the provider claimed. This package owns the claims and
// the meaning of the values. It turns every fragment that belongs to one table
// into a single desired row-table facet, because a document may carry only one
// facet of a kind for a table.
//
// What the bundled decoder in the host reads and this package does not is
// listed in the package README of the provider and in the tests: starting
// partition layouts, column family cache modes and column placement, TTL,
// changefeeds, column storage and index options. The host refuses a YAML key
// that no provider claims, and reads a Go comment that none claims as no
// declaration. So the provider leaves unclaimed the YAML keys it cannot carry,
// and claims the Go directives it cannot carry in order to refuse them by name.
// A claimed key or attribute that it cannot carry yet is refused here by name
// too.
package tablesource

import (
	"slices"

	"github.com/stokaro/ptah-ydb-plugin/schema"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// The keys and attributes that set a table's partitioning, spelled as YDB
// spells the settings, in lower case. A YAML table entry and a table-scope Go
// directive read the same names.
const (
	AttributeBySize          = "auto_partitioning_by_size"
	AttributePartitionSizeMB = "auto_partitioning_partition_size_mb"
	AttributeByLoad          = "auto_partitioning_by_load"
	AttributeMinPartitions   = "auto_partitioning_min_partitions_count"
	AttributeMaxPartitions   = "auto_partitioning_max_partitions_count"
	AttributeReadReplicas    = "read_replicas_settings"
	AttributeKeyBloomFilter  = "key_bloom_filter"
)

// The keys and attributes that declare a column family.
const (
	// KeyColumnFamilies is the YAML table key that holds a mapping of family
	// names to family settings.
	KeyColumnFamilies = "column_families"
	// DirectiveColumnFamily declares one family of the struct's table, or of
	// the table its table attribute names.
	DirectiveColumnFamily = "ptah:schema:columnfamily"
	// DirectiveChangefeed and DirectiveChangefeedConsumer are claimed so that
	// the provider can refuse them by name. The host reads a Go comment that
	// no provider claims as no declaration at all, so leaving them unclaimed
	// would drop a changefeed without a word.
	DirectiveChangefeed         = "ptah:schema:changefeed"
	DirectiveChangefeedConsumer = "ptah:schema:changefeed:consumer"

	AttributeName        = "name"
	AttributeTable       = "table"
	AttributeData        = "data"
	AttributeCompression = "compression"
	// AttributeCacheMode and AttributeFields are claimed so that the provider
	// can refuse them by name: the row-table model has no cache mode, and a
	// facet cannot attach to a column yet.
	AttributeCacheMode = "cache_mode"
	AttributeFields    = "fields"
)

// partitioningKeys are the table keys that set partitioning, in the order a
// refusal reports them.
func partitioningKeys() []string {
	return []string{
		AttributeBySize, AttributePartitionSizeMB, AttributeByLoad, AttributeMinPartitions,
		AttributeMaxPartitions, AttributeReadReplicas, AttributeKeyBloomFilter,
	}
}

// YAMLKeys returns the table-scope YAML keys the provider claims. Each key
// declares the row-table properties model.
func YAMLKeys() []*protocol.YAMLSourceKey {
	keys := slices.Concat(partitioningKeys(), []string{KeyColumnFamilies})
	claims := make([]*protocol.YAMLSourceKey, 0, len(keys))
	for _, key := range keys {
		claims = append(claims, &protocol.YAMLSourceKey{Scope: protocol.YAMLSourceKey_TABLE, Key: key, Selector: key, Models: []string{schema.TableProperties}})
	}
	return claims
}

// GoDirectives returns the table-scope Go directives the provider claims. The
// attributes of the changefeed directives are the ones the host's bundled
// decoder reads, claimed only so that a changefeed is refused by name.
func GoDirectives() []*protocol.GoSourceDirective {
	claim := func(directive string, attributes ...string) *protocol.GoSourceDirective {
		return &protocol.GoSourceDirective{Scope: protocol.GoSourceDirective_TABLE, Directive: directive, Selector: directive, Attributes: attributes,
			Models: []string{schema.TableProperties}}
	}
	return []*protocol.GoSourceDirective{
		claim(DirectiveColumnFamily, AttributeName, AttributeTable, AttributeData, AttributeCompression, AttributeCacheMode, AttributeFields),
		claim(DirectiveChangefeed, AttributeName, AttributeTable, "mode", "format", "virtual_timestamps", "resolved_timestamps", "initial_scan", "user_sids",
			"schema_changes", "topic_min_active_partitions", "topic_auto_partitioning", "retention_period"),
		claim(DirectiveChangefeedConsumer, AttributeName, "changefeed", AttributeTable, "important", "read_from", "supported_codecs", "availability_period"),
	}
}

// Owns reports whether the fragment carries one of the claims of this package.
func Owns(fragment *protocol.SourceFragment) bool {
	if key := fragment.GetYamlKey(); key != nil {
		return key.GetScope() == protocol.YAMLSourceKey_TABLE && slices.ContainsFunc(YAMLKeys(), func(claim *protocol.YAMLSourceKey) bool { return claim.GetKey() == key.GetKey() })
	}
	if directive := fragment.GetGoDirective(); directive != nil {
		return directive.GetScope() == protocol.GoSourceDirective_TABLE &&
			slices.ContainsFunc(GoDirectives(), func(claim *protocol.GoSourceDirective) bool { return claim.GetDirective() == directive.GetDirective() })
	}
	return false
}
