// Package tablesource decodes the table-scope declarations a YAML or Go
// annotation source writes for a YDB table: a row table's automatic
// partitioning, read replicas, key bloom filter, starting layout, TTL and column
// families, and a column table's storage, hash key, shard count and TTL.
//
// The host lexes the source and sends the provider the parsed values of the
// keys, directives and attributes the provider claimed. A Go source writes the
// partitioning, the storage and the TTL as attributes of the host's own
// ptah:schema:table directive, and a column family as a directive of its own.
// This package owns the claims and the meaning of the values. It turns every
// fragment that belongs to one table into a single desired facet, the row-table
// facet or the column-storage facet, because a document may carry only one
// facet of a kind for a table.
//
// What the bundled decoder in the host reads and this package does not is
// listed in the package README of the provider and in the tests: index
// options. A table's changefeeds are decoded by internal/changefeedsource.
// The host refuses a YAML key that no provider claims, and reads a Go comment
// that none claims as no declaration. So the provider leaves unclaimed the
// YAML keys it cannot carry, and claims the Go directives and attributes it
// cannot carry in order to refuse them by name. A claimed key or attribute
// that it cannot carry yet is refused here by name too.
package tablesource

import (
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-ydb-plugin/schema"
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
	// new table starts with. YDB takes it only when it creates the table.
	AttributeUniformPartitions = "uniform_partitions"
	AttributePartitionAtKeys   = "partition_at_keys"

	// The attributes that choose a column table and describe it. A row table
	// states store=ROW or none of them.
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

// HCLPlatformBlock returns the claim of a table's platform "ydb" block in HCL
// (contract 25.22, source.hcl-platform): its row_deletion_column,
// row_deletion_interval and row_deletion_unit overrides, which declare the
// same TTL as the Go attributes and the YAML group above. The host refuses an
// override the claim does not list, so the provider reads every override the
// group holds. The row-table and column-storage models list no HCL: a table
// that writes no block states nothing about its TTL, and the TTL it holds
// stays, as on the host's embedded engine.
//
// The block is also where an export writes what a table states by writing
// nothing, as the Go table directive is: no changefeed, no native index, and
// a column of the default family. A read reports an empty set of each for
// every table that has none, and a facet for every column, and without a
// claim of their models the host could only report them as details the
// document leaves out. The block decodes to none of them, so a table keeps
// the changefeeds, the native indexes and the column families it holds.
func HCLPlatformBlock() *protocol.HCLSourceBlock {
	attributes := make([]*protocol.HCLSourceAttribute, 0, len(ttlAttributes()))
	for _, name := range ttlAttributes() {
		attributes = append(attributes, &protocol.HCLSourceAttribute{Name: strings.TrimPrefix(name, PlatformPrefix), Kind: protocol.HCLSourceAttribute_STRING})
	}
	return &protocol.HCLSourceBlock{Site: protocol.HCLSourceBlock_TABLE_PLATFORM, Type: "ydb", Selector: KeyPlatformYDB, Attributes: attributes,
		Models: []string{schema.TableProperties, schema.ColumnStorageProperties, schema.ChangefeedProperties, schema.IndexProperties, schema.ColumnProperties}}
}

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

	AttributeName        = "name"
	AttributeTable       = "table"
	AttributeData        = "data"
	AttributeCompression = "compression"
	// AttributeCacheMode is the family's cache mode, regular or in_memory in
	// any case. AttributeFields lists the columns of the family in either a Go
	// directive or a YAML mapping.
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

// layoutKeys are the table keys that set the starting layout of a new table, in
// the order a refusal reports them.
func layoutKeys() []string { return []string{AttributeUniformPartitions, AttributePartitionAtKeys} }

// tableAttributes are the attributes the provider claims on ptah:schema:table,
// in the order a refusal reports them.
func tableAttributes() []string {
	return slices.Concat(partitioningKeys(), layoutKeys(), []string{
		AttributeStore, AttributeHash, AttributeShards, AttributeColumnTTL, AttributeTTLColumn, AttributeTTLInterval, AttributeTTLUnit})
}

// indexAttributes are the attributes the provider claims on ptah:schema:index:
// the ones that set a global index's partitioning and read replicas, which
// declare the index's own facet, and the local, full-text and vector options,
// which the provider refuses by name.
func indexAttributes() []string {
	return slices.Concat(IndexSettingsAttributes(), refusedIndexAttributes())
}

// IndexSettingsAttributes are the attributes, and the YAML index keys, that
// set the partitioning and the read replicas of a global sync, async or
// unique index, in the order an export writes them, as the host's embedded
// engine writes them.
func IndexSettingsAttributes() []string {
	return []string{AttributeBySize, AttributePartitionSizeMB, AttributeByLoad, AttributeMinPartitions, AttributeMaxPartitions, AttributeReadReplicas}
}

// refusedIndexAttributes are the index options the bundled decoder in the host
// reads and the provider does not carry from a source: a column table's local
// index options, and the full-text and vector settings, which the native
// index facet states.
func refusedIndexAttributes() []string {
	return []string{
		"false_positive_probability", "ngram_size", "case_sensitive", "tokenizer", "language",
		"use_filter_lowercase", "use_filter_stopwords", "use_filter_ngram", "use_filter_edge_ngram", "filter_ngram_min_length", "filter_ngram_max_length",
		"use_filter_length", "filter_length_min", "filter_length_max", "use_filter_snowball",
		"distance", "similarity", "vector_type", "vector_dimension", "levels", "clusters",
	}
}

// YAMLKeys returns the table-scope YAML keys the provider claims. A key
// declares the row-table properties model; column_store declares the
// column-storage model, and the TTL of the ydb platform group is a column
// table's TTL beside it, so the group declares both.
func YAMLKeys() []*protocol.YAMLSourceKey {
	keys := slices.Concat(partitioningKeys(), layoutKeys(), []string{KeyColumnFamilies, KeyPlatformYDB, KeyColumnStore})
	claims := make([]*protocol.YAMLSourceKey, 0, len(keys))
	for _, key := range keys {
		models := []string{schema.TableProperties}
		switch key {
		case KeyColumnFamilies:
			// The families place columns. A table entry states a column of
			// the default family, an empty native index set and an index
			// holding the settings YDB gives a new one by writing nothing,
			// and the families key is where an export writes that.
			models = []string{schema.TableProperties, schema.ColumnProperties, schema.IndexProperties, schema.IndexSettingsProperties}
		case KeyColumnStore:
			models = []string{schema.ColumnStorageProperties}
		case KeyPlatformYDB:
			models = []string{schema.TableProperties, schema.ColumnStorageProperties}
		}
		claims = append(claims, &protocol.YAMLSourceKey{Scope: protocol.YAMLSourceKey_TABLE, Key: key, Selector: key, Models: models})
	}
	return claims
}

// IndexYAMLKeys returns the index-entry YAML keys the provider claims
// (contract 25.3, under source.entry-keys): the index directive's attributes,
// which a YAML index entry spells alike, as the host's bundled YAML source
// reads them. A partitioning or read replica key declares the index's own
// facet; any other is claimed so that it is refused by name, as the
// directive's attributes are.
func IndexYAMLKeys() []*protocol.YAMLSourceKey {
	var claims []*protocol.YAMLSourceKey
	for _, key := range indexAttributes() {
		models := []string{schema.TableProperties}
		if slices.Contains(IndexSettingsAttributes(), key) {
			models = []string{schema.IndexSettingsProperties}
		}
		claims = append(claims, &protocol.YAMLSourceKey{Scope: protocol.YAMLSourceKey_INDEX, Key: key, Selector: "index." + key, Models: models})
	}
	return claims
}

// GoDirectives returns the Go claims of this package: the attributes it adds
// to the host's table and index directives, and the column family directive.
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
	index.Scope, index.Models = protocol.GoSourceDirective_INDEX, []string{schema.IndexSettingsProperties}
	// The table directive declares a row table's settings or a column table's
	// storage. It is also where a table states what it states by writing
	// nothing: a column of the default family, no changefeed, no native index
	// and an index holding the settings YDB gives a new one, which an export
	// writes as the directive's part.
	table := claim(DirectiveTable, tableAttributes()...)
	table.Models = append(table.Models, schema.ColumnStorageProperties, schema.ColumnProperties, schema.ChangefeedProperties, schema.IndexProperties,
		schema.IndexSettingsProperties)
	return []*protocol.GoSourceDirective{
		table,
		index,
		family,
	}
}

// HCLIndexBlock returns the HCL claim of this package: the attributes that set
// a global index's partitioning and read replicas inside the frontend's index
// block, as the host's embedded engine writes them there. A switch and the
// read replicas are strings, and a count a number, read as its text.
func HCLIndexBlock() *protocol.HCLSourceBlock {
	block := &protocol.HCLSourceBlock{Site: protocol.HCLSourceBlock_INDEX_ATTRIBUTES, Type: "index", Selector: "index", Models: []string{schema.IndexSettingsProperties}}
	for _, name := range IndexSettingsAttributes() {
		block.Attributes = append(block.Attributes, &protocol.HCLSourceAttribute{Name: name, Kind: protocol.HCLSourceAttribute_TEXT})
	}
	return block
}

// Owns reports whether the fragment carries one of the claims of this package.
func Owns(fragment *protocol.SourceFragment) bool {
	if key := fragment.GetYamlKey(); key != nil {
		return slices.ContainsFunc(slices.Concat(YAMLKeys(), IndexYAMLKeys()), func(claim *protocol.YAMLSourceKey) bool {
			return claim.GetScope() == key.GetScope() && claim.GetKey() == key.GetKey()
		})
	}
	if directive := fragment.GetGoDirective(); directive != nil {
		return slices.ContainsFunc(GoDirectives(), func(claim *protocol.GoSourceDirective) bool {
			return claim.GetDirective() == directive.GetDirective() && claim.GetScope() == directive.GetScope()
		})
	}
	// An HCL fragment is this package's when it is one of the blocks the
	// package declares, an index's attributes or a table's platform "ydb"
	// block. Both are recognized here, so neither reaches the coordination
	// decoder, which refuses any other block.
	if block := fragment.GetHclBlock(); block != nil {
		return slices.ContainsFunc([]*protocol.HCLSourceBlock{HCLIndexBlock(), HCLPlatformBlock()}, func(claim *protocol.HCLSourceBlock) bool {
			return claim.GetSite() == block.GetSite() && claim.GetType() == block.GetType()
		})
	}
	return false
}
