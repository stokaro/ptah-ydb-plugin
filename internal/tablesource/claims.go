// Package tablesource decodes the table-scope declarations a YAML or Go
// annotation source writes for a YDB row table: its automatic partitioning,
// read replicas, key bloom filter, TTL and column families.
//
// The host lexes the source and sends the provider the parsed values of the
// keys, directives and attributes the provider claimed. A Go source writes the
// partitioning and the TTL as attributes of the host's own ptah:schema:table
// directive, and a column family as a directive of its own. This package owns
// the claims and the meaning of the values. It turns every fragment that
// belongs to one table into a single desired row-table facet, because a
// document may carry only one facet of a kind for a table.
//
// What the bundled decoder in the host reads and this package does not is
// listed in the package README of the provider and in the tests: starting
// partition layouts, column family cache modes, the columns of a family written
// in YAML, changefeeds, column storage and index options. The host
// refuses a YAML key that no provider claims, and reads a Go comment that none
// claims as no declaration. So the provider leaves unclaimed the YAML keys it
// cannot carry, and claims the Go directives and attributes it cannot carry in
// order to refuse them by name. A claimed key or attribute that it cannot carry
// yet is refused here by name too.
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

// The attributes of the host's own ptah:schema:table directive that a provider
// may claim. Partitioning and the starting layout of a new table, column
// storage and the TTL are the ones the bundled decoder reads.
const (
	// DirectiveTable is the host's directive that declares a table.
	DirectiveTable = "ptah:schema:table"

	// AttributeUniformPartitions and AttributePartitionAtKeys set the layout a
	// new table starts with. They are claimed so that they are refused by name.
	AttributeUniformPartitions = "uniform_partitions"
	AttributePartitionAtKeys   = "partition_at_keys"

	// The attributes that choose a column table and describe it. A row table
	// has none; they are claimed so that a column table is refused by name.
	AttributeStore     = "store"
	AttributeHash      = "partition_by_hash"
	AttributeShards    = "column_shards"
	AttributeColumnTTL = "column_ttl"

	// The platform properties that carry a row table's TTL, spelled as the host
	// spells a platform property of the ydb engine.
	AttributeTTLColumn   = PlatformPrefix + "row_deletion_column"
	AttributeTTLInterval = PlatformPrefix + "row_deletion_interval"
	AttributeTTLUnit     = PlatformPrefix + "row_deletion_unit"
)

// PlatformPrefix starts the name of a Go attribute that sets a platform
// property of the ydb engine.
const PlatformPrefix = "platform.ydb."

// KeyPlatformYDB is the YAML table key that claims the ydb group of a table's
// platform key. The host hands the provider the mapping of the group with its
// keys unprefixed, so a table that writes
//
//	platform:
//	  ydb: {row_deletion_column: created_at, row_deletion_interval: PT1H}
//
// declares the same TTL as the three Go attributes above. The host keeps no copy
// of the group, so every key the provider does not read is refused by name.
const KeyPlatformYDB = "platform.ydb"

// DirectiveIndex is the host's directive that declares an index. The bundled
// decoder reads partitioning, vector and full-text options on it.
const DirectiveIndex = "ptah:schema:index"

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
	// AttributeCacheMode is claimed so that the provider can refuse it by name:
	// the row-table model has no cache mode. AttributeFields lists the columns
	// of the family. A Go directive places them; a YAML family cannot, because
	// the host attaches a YAML value to a table only.
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

// tableAttributes are the attributes the provider claims on ptah:schema:table,
// in the order a refusal reports them.
func tableAttributes() []string {
	return slices.Concat(partitioningKeys(), []string{AttributeUniformPartitions, AttributePartitionAtKeys,
		AttributeStore, AttributeHash, AttributeShards, AttributeColumnTTL, AttributeTTLColumn, AttributeTTLInterval, AttributeTTLUnit})
}

// indexAttributes are the attributes the provider claims on ptah:schema:index:
// the ones that set an index's partitioning, local options, full-text options
// and vector settings. The provider refuses every one by name.
func indexAttributes() []string {
	return []string{
		AttributeBySize, AttributePartitionSizeMB, AttributeByLoad, AttributeMinPartitions, AttributeMaxPartitions, AttributeReadReplicas,
		"false_positive_probability", "ngram_size", "case_sensitive", "tokenizer", "language",
		"use_filter_lowercase", "use_filter_stopwords", "use_filter_ngram", "use_filter_edge_ngram", "filter_ngram_min_length", "filter_ngram_max_length",
		"use_filter_length", "filter_length_min", "filter_length_max", "use_filter_snowball",
		"distance", "similarity", "vector_type", "vector_dimension", "levels", "clusters",
	}
}

// YAMLKeys returns the table-scope YAML keys the provider claims. Each key
// declares the row-table properties model.
func YAMLKeys() []*protocol.YAMLSourceKey {
	keys := slices.Concat(partitioningKeys(), []string{KeyColumnFamilies, KeyPlatformYDB})
	claims := make([]*protocol.YAMLSourceKey, 0, len(keys))
	for _, key := range keys {
		claims = append(claims, &protocol.YAMLSourceKey{Scope: protocol.YAMLSourceKey_TABLE, Key: key, Selector: key, Models: []string{schema.TableProperties}})
	}
	return claims
}

// GoDirectives returns the Go claims of the provider: the attributes it adds to
// the host's table and index directives, and the directives of its own. The
// attributes of the changefeed directives are the ones the host's bundled
// decoder reads, claimed only so that a changefeed is refused by name.
func GoDirectives() []*protocol.GoSourceDirective {
	claim := func(directive string, attributes ...string) *protocol.GoSourceDirective {
		return &protocol.GoSourceDirective{Scope: protocol.GoSourceDirective_TABLE, Directive: directive, Selector: directive, Attributes: attributes,
			Models: []string{schema.TableProperties}}
	}
	// A column family lists the columns it holds, and each column carries the
	// family it sits in, so the directive also answers for column properties.
	family := claim(DirectiveColumnFamily, AttributeName, AttributeTable, AttributeData, AttributeCompression, AttributeCacheMode, AttributeFields)
	family.Models = append(family.Models, schema.ColumnProperties)
	index := claim(DirectiveIndex, indexAttributes()...)
	index.Scope = protocol.GoSourceDirective_INDEX
	return []*protocol.GoSourceDirective{
		claim(DirectiveTable, tableAttributes()...),
		index,
		family,
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
		return slices.ContainsFunc(GoDirectives(), func(claim *protocol.GoSourceDirective) bool {
			return claim.GetDirective() == directive.GetDirective() && claim.GetScope() == directive.GetScope()
		})
	}
	return false
}
