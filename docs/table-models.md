---
title: YDB table source and model conversion
description: Offline row-table declarations, column and index models, and conversion rules.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How do YDB table source declarations become provider models?"
goal: "Understand YDB table declarations and offline model conversion."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 10
---

# YDB table source and model conversion

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Offline row-table declarations

The same source service decodes the table-scope settings that the host's
bundled YDB source reads from YAML and from Go annotations, so a table written
the way users write tables reaches the provider. The host lexes the source and
sends the values of the keys, directives and attributes the provider claimed;
the provider owns their meaning. The host refuses a YAML key the provider does
not claim. It reads a Go comment that no provider claims as no declaration, and
refuses an attribute of its own directives that no provider claims, so the
provider claims the Go directives and attributes it cannot carry and refuses
them by name. Nothing is dropped in silence.

A YAML table entry takes these keys, spelled as the YDB settings are:

```yaml
tables:
  documents:
    auto_partitioning_by_size: ENABLED          # ENABLED or DISABLED, in either case
    auto_partitioning_partition_size_mb: 2048   # a whole number of at least 1
    auto_partitioning_by_load: ENABLED
    auto_partitioning_min_partitions_count: 2
    auto_partitioning_max_partitions_count: 16
    read_replicas_settings: ANY_AZ:2            # PER_AZ:<n> or ANY_AZ:<n>; a count of 0 removes replicas
    key_bloom_filter: ENABLED
    column_families:
      default: {compression: lz4}               # off or lz4
      cold: {data: hdd, compression: lz4, fields: [body]} # data is a storage pool kind, kept as written
      hot: {cache_mode: in_memory}              # regular or in_memory, in either case
    platform:
      ydb:                                      # the TTL, spelled as the Go platform properties without the prefix
        row_deletion_column: created_at
        row_deletion_interval: P30D
```

In YAML, a family's `fields` lists its column names as a sequence or a
comma-separated scalar. Each name must match a column in that table. A column
can belong to only one family, and the default family lists no columns.

The provider claims the `ydb` group of the table's `platform` key as
`platform.ydb`, and the host keeps no copy of the group. The provider therefore
refuses every key of the group other than `row_deletion_column`,
`row_deletion_interval` and `row_deletion_unit`. The host refuses a group that
holds a list or a mapping before it reaches the provider; scalar values arrive
as written.

In Go, the same settings are attributes of the host's own `ptah:schema:table`
directive, spelled as the YAML keys are, and a TTL is written as the platform
properties of the `ydb` engine. A family is a directive of its own,
`//ptah:schema:columnfamily`, on the table's struct, with the attributes `name`,
`data`, `compression`, `cache_mode` and `fields`. The `table` attribute names
another table of the file (`name` or `schema.name`) and wins over the struct's
own table.
`fields` lists the columns the family holds, as a comma-separated list of the
column names (`fields="body, notes"`). The provider returns one column facet for
each name, owned by the column under the table, and the host attaches it to the
field of the struct the directive is written on.

A family that has `fields` and
a `table` attribute for another table is refused, because the host places only
the columns of the directive's own struct. The default family lists no column,
a column sits in one family, and a name the struct has no field for is refused by
the host.

```go
//ptah:schema:table name="documents" auto_partitioning_by_load="ENABLED" read_replicas_settings="ANY_AZ:2" platform.ydb.row_deletion_column="created_at" platform.ydb.row_deletion_interval="P30D"
//ptah:schema:columnfamily name="default" compression="lz4"
//ptah:schema:columnfamily name="cold" data="hdd" compression="lz4" fields="body"
//ptah:schema:columnfamily name="hot" cache_mode="in_memory"
type Document struct { /* ... */ }
```

A family's cache mode, `regular` or `in_memory`, needs a target that takes
one: YDB 25.4 and later, or a cluster that turned `EnableTableCacheModes` on
(see [Feature flags](connections-and-execution.md#feature-flags)). A plan that declares one for any other
target is refused with the flag's name, `regular` included, as the host's
embedded engine refuses it.

In HCL, the TTL is the `platform "ydb"` block of the table, with the same
three properties as `override` blocks. `ptah` and `ptah-compat` read it and
`schema inspect` writes it:

```hcl
table "events" {
  schema = schema.app
  platform "ydb" {
    override "row_deletion_column" {
      value = "created_at"
    }
    override "row_deletion_interval" {
      value = "P30D"
    }
  }
  column "id" {
    type = Uint64
  }
  column "created_at" {
    type = Timestamp
    null = true
  }
  primary_key {
    columns = [column.id]
  }
}
```

The provider claims the block (`source.hcl-platform`, ADR 0072), so it needs a
host that reads it; a host that does not refuses the document. The host refuses
an override other than the three, naming them. The block's label may be the
alias `ydbs`. A table that writes no block states nothing about its TTL, and
keeps the TTL it holds, where a YAML or Go source that states no TTL asks for
none. HCL cannot say a table is a column table, so the block states the TTL of
a column table the database holds, which keeps its storage and takes the TTL as
one tier that deletes.

A TTL needs `row_deletion_column` and `row_deletion_interval`. The interval is an
ISO 8601 duration of weeks, days, hours, minutes and seconds (`P30D`,
`P2W3D`, `PT1H30M`), and YDB keeps whole seconds, so a fraction of a second,
months and years are refused. A column that counts since the Unix epoch also
needs `row_deletion_unit`, one of `SECONDS`, `MILLISECONDS`, `MICROSECONDS` and
`NANOSECONDS` in any case. Which column types fit the unit is checked when the
plan is made, where the table's columns are known.

A refused interval or unit
says why in the host's output, in the words of the host's embedded engine
without the value the source wrote, which a provider's reason never quotes:
`table "items": platform.ydb.row_deletion_interval: the interval has a fraction
of a second, and YDB keeps whole seconds only (PT1.5S reads back as PT1S)`. A
tier of a column table's TTL is refused the same way.

All the keys, attributes and directives of one table become one desired
row-table facet, because a document may hold only one facet of a kind for a
table. The facet holds only what the source states. It has no primary key, which
comes from the table's common columns; a stated key must still be distinct and
ordered. A partitioning setting a source leaves out keeps what the table holds.
A table that declares nothing, such as an empty `column_families` mapping or
`store="ROW"` alone, gets no facet.

The values are read as the text the author wrote, so `3` and `"3"` are the
same count, and a boolean where `ENABLED` is due is refused. The shared
frontend normalizes the spelling of a YAML number before the provider sees it
(`1_000` arrives as `1000`, `0x10` as `16`); the bundled decoder reads the
written text and refuses both.

A starting layout is the partitions a new table is created with, and YDB takes
it only when it creates the table. `uniform_partitions` takes a count of at
least 1, and `partition_at_keys` takes split points in the host's grammar: a
comma separated list whose items are a value, for a split on the first key
column, or a parenthesized list of values for the leading key columns, such as
`10, 20` or `(10, 'a'), (20)`. A value is a bare run of characters or a string in
single or double quotes with backslash escapes. Both keys are claimed in YAML
and in the Go table directive, and a table that names both is refused where it
is decoded; the host accepts the pair when it decodes it and refuses the table
when it plans it.

The key column types are checked when the table is created
(see "Table creation"). The corpus holds the inputs and the host's answers.

The Go `fields` directive with a `table` attribute for another table is not
read. The host places only the columns of the directive's own struct. The
provider claims and refuses this attribute by name.

### Index options on other index types

A host that lists `source.index-kinds` sends a vector, full-text, or local
index whole; see [index declarations of the provider's types](indexes-and-table-execution.md#index-declarations-of-the-providers-types).
For an index of another type, and for every index on a host without that
feature, the provider claims and refuses these `ptah:schema:index` attributes
and the corresponding YAML keys by name.

A global index's partitioning and read replicas are read; see [index settings
from a source](indexes-and-table-execution.md#index-settings-from-a-source).

A table is managed for its TTL when its source states table properties for it,
or when the host reports complete knowledge of the table properties of the
source (the knowledge a Go or YAML source carries when its format describes
them). A comparison that finds a TTL on such a table and none declared reports a
change, and the planner removes the TTL, as the bundled source does: measured
with the host at the commit named in the corpus, `schema apply` over a Go file
with no TTL plans `ALTER TABLE ... RESET (TTL)` for every table of the file. A
source that could not describe the properties, or a table whose current
properties were not read, leaves the TTL alone.

### Column tables

A column table is declared with `store="COLUMN"` in the Go table directive (in
either case; an export writes `store="column"`, as the host's embedded engine
did), or
with the `column_store` mapping in YAML. The Go attributes `partition_by_hash`
(a comma separated list of key columns), `column_shards` (a positive 32-bit
count) and `column_ttl`, and the YAML keys `hash_columns`, `partitions` and
`ttl`, state the hash key, the shard count and a tiered TTL. A tiered TTL names
a column, an optional epoch unit and ordered tiers; only the last tier may
delete rows, and every other one moves them to an absolute external data source
path.

A TTL that only deletes is written as the row TTL
(`platform.ydb.row_deletion_*` in Go, the `row_deletion_*` keys of the YAML `ydb`
group) and becomes the column table's one deleting tier. The declaration
becomes one column-storage facet, and what it leaves out stays the server's
choice. `store="ROW"` declares nothing.

The corpus holds each spelling and each refusal with the host's answer
(`*-store-column-*` rows). The host's bundled decoders also accept a column
table beside row-table settings (partitioning, read replicas, a key bloom
filter, a starting layout, column families) and beside both a tiered TTL and a
row TTL, and its renderer refuses the table when it plans it. The provider
refuses such a declaration where it is decoded, with the renderer's words:
`row-table partitioning, column families and changefeeds cannot be applied to a
column table`, and `declare either tiered TTL or a TTL that deletes rows, not
both`. It is stricter in one place: the host reads a fractional YAML
`partitions` as its whole part, and the provider refuses it.

Inspection reads a column table, a `COLUMN_TABLE` directory entry, from the
table service and from the monitoring endpoint's scheme description
(`/viewer/json/describe`). The table service's description carries the
columns, the key, the hash key (`partitioning_settings.partition_by`) and the
TTL, and leaves out the shard count and the local indexes (measured on 25.1.4.7
and 26.2.1.14 for a table created with `AUTO_PARTITIONING_MIN_PARTITIONS_COUNT
= 3`); the monitoring page carries both. The two are read together and a
disagreement refuses the inspection.

The table service writes a tiered TTL in
a field the pinned `ydb-go-genproto` lacks, so the provider reads a column
table's description with definitions of its own (`ColumnTableDescribeResult`,
adapted from upstream with its wire numbers). The table reads back as a common
table with one column-storage facet (hash key, shard count, data channel
count, local indexes and the TTL with its tiers, a TTL that only deletes
included) and a column facet for each column; it has no row-table, index or
changefeed facet. Its access metadata is not observed. A column table with a
column family, a column default, attributes or row-table settings is refused.

The hash function has to be the one YQL's `PARTITION BY HASH` writes,
`HASH_FUNCTION_CONSISTENCY_64`: the native `CreateTable` call writes
`HASH_FUNCTION_MODULO_N` instead, and the host's embedded engine reads no other.

Without the monitoring parameter in the address, an inspection that meets a
column table is refused with `column table <path> requires
monitoring=http(s)://host:port in the YDB address`, and the column table is
never read as a row table. The SDK client passes the host the status code of a
refused call and not its message, so the host reports `provider inspect failed
(FailedPrecondition)`.

Creation. The planner creates a declared column table with one YQL `CREATE
TABLE ... PARTITION BY HASH (...) WITH (STORE = COLUMN, ...)` through the query
service, once, outside a transaction, with the shard count as
`AUTO_PARTITIONING_MIN_PARTITIONS_COUNT` and the TTL as YQL writes it. YQL is
used rather than the native `CreateTable` call because the native call writes
`HASH_FUNCTION_MODULO_N`, which the host's embedded engine does not read. Every
key column is written NOT NULL, as the host writes it: YDB refuses a nullable
key column in a column table, and a Go field marked primary is nullable unless
it says `not_null`.

The table is read back from the table service and the
monitoring endpoint and must hold the declared columns and key, the declared
hash key or, without one, the whole key, the declared shard count, the declared
TTL and the declared local indexes. A creation in a session whose address names
no monitoring endpoint is refused before anything is written. A tier that moves
rows needs the `tiered_ttl` capability, which a session reads from
`EnableTieringInColumnShard`; without it the plan is refused with the reason.
Column defaults, Serial columns, column families, global indexes and a data
channel count other than the server's 64 are refused.

A TTL column has to be
first in the key: the host's embedded engine also takes one later in the key
that a min_max local index reads, which 26.2.1.14 refuses with the index in
place (`Haven't MAX-index for TTL column and TTL column is not first column in
primary key`), so the provider refuses it before a statement is sent.

Local indexes. The column-storage facet states a column table's local indexes
as its read describes them: a `bloom` (`bloom_filter`), an `ngram`
(`bloom_ngram_filter`) or a `minMax` (`min_max`) index, each over one column,
with its settings. A setting the declaration leaves out is the server's
default: a probability of 0.1, an n-gram size of 3 and case-sensitive
matching. The storage YDB places an index in is not part of a declaration, and
the comparison leaves it out, since YDB describes an index created with its
table in other storage than one added later.

An index reads one column
(26.2.1.14 answers `Local bloom index on column tables requires exactly one
index column`, where the host's embedded engine takes several for a bloom
index), an n-gram index a `Utf8` one, an n-gram size is 3 to 8, and a
probability gives at most 8 n-gram hashes, as that engine requires. Each kind
needs its capability (see [Feature flags](connections-and-execution.md#feature-flags)); 25.1 has no local
index grammar at all (`local: alternative is not implemented yet`). A new
table's local indexes are written inside its CREATE TABLE, as ``INDEX `bf`
LOCAL USING bloom_filter ON (`body`) WITH (false_positive_probability =
0.01)``, every setting named.

A Go or YAML source declares a local index as an
index directive or entry of its type, which a host that lists
`source.index-kinds` sends whole (see [Index declarations of the provider's
types](indexes-and-table-execution.md#index-declarations-of-the-providers-types)); an HCL document declares
none, since HCL cannot state the column table it belongs to.

Export. A Go or YAML export of a column table writes its hash key, its shard
count and its TTL, and on a host that lists `source.index-kinds` each local
index as a declaration of its type with its settings. A source writes no data
channel count, and the 64 YDB gives every column table the provider creates is
what a declaration without one creates, so a read's count of 64 is no loss; any
other count is. The storage and the encodings YDB gives a local index are not
part of a declaration either.

Comparison and planning. The comparison `ydb-row-table-properties` compares the
column-storage facet of a table that exists on both sides, and reports a
`github.com/stokaro/ptah-ydb-plugin/column-storage-change` when the hash key,
the shard count, the TTL or the local indexes differ, or when the table is a
column table on one side and a row table on the other. A declaration that
leaves out the hash key or the shard count takes what the table holds. Under
complete coverage, a table the source states no column storage for asks for a
row table.

YDB fixes a table's storage kind, hash key and shard count when it
creates the table and copies no column table into a new one, so the planner
refuses each of these changes with the host's words (`changing between row and
column storage requires an explicit data migration`, `changing a column
table's hash key or shard count requires an explicit data migration`, with both
values; `column-table rebuilds require an explicit data migration`). A column step of
a column table is refused as not planned yet.

The local indexes are managed when the declaration states that it lists every
one, as a read does and as a Go or YAML source of a host that lists
`source.index-kinds` does (`localIndexesStated`), or when it states at least
one: an index the table holds and the declaration leaves out is dropped. Any
other declaration that states none leaves the table's own alone, since its
source cannot declare one. Each change is one step in the lifecycle phase,
`column-table-add-local-index/N`, `column-table-drop-local-index/N` or
`column-table-rename-local-index/N`, of the operation kind
`column-table-local-index-operation`, running `ALTER TABLE ... ADD INDEX ...
LOCAL USING ...`, `ALTER TABLE ... DROP INDEX ...` or `ALTER TABLE ... RENAME
INDEX ... TO ...` through the query service.

An index whose kind, column or
settings differ is dropped and added again under its name, the removal first.
An index whose name alone changes is renamed in place on a target with
`index_rename`, and dropped under its old name and added under its new one on
a target without it, as the host's embedded engine plans both. The host
compares no local index, so the provider pairs them as the host's comparison
pairs a renamed index: each new name, in name order, takes the first old name
in name order whose index has the same column, kind and settings.

The session
reports `index_rename` from `EnableMoveIndex`, and 26.2.1.14 renames a local
index with the flag off too, so a plan without it rebuilds an index the server
could rename. 26.2.1.14 also changes a bloom index's probability in place
(`ALTER INDEX ... SET (false_positive_probability = ...)`), which neither
engine plans. Only an index a plan adds needs its capability.

Execution reads
the table from the table service and the monitoring endpoint and refuses
before it writes when the table holds an index of the name to add or to rename
to, lacks its column or holds it with another type than an n-gram index reads,
or holds the index to drop or rename otherwise than the plan captured it;
after the call it requires the added or renamed index to read back as
requested under its name, the dropped or renamed one to be gone under its old
name, and every other local index, the layout and the TTL to be unchanged.

The TTL changes in place, as the host's embedded engine changes it. `ALTER
TABLE ... SET (TTL = ...)` replaces the TTL a column table holds, whatever
either one moves rows to (measured on 25.1.4.7 and 26.2.1.14), so a held TTL
that moves no row is replaced by the SET alone (`column-table-ttl/<n>/set`). A
held TTL that moves rows to an external data source is removed first with
`ALTER TABLE ... RESET (TTL)` (`column-table-ttl/<n>/reset`, placed early and
ordered before the SET): YDB refuses to drop or replace a data source a TTL
names (`Other entities depend on this data source`), so the held TTL lets go of
its sources before their owner drops or replaces them, and the declared one is
set once they exist.

Each step reads the data sources its TTL names, which
orders it against their owner. A table without a TTL takes the SET alone, and a
declaration without one the RESET alone. A lone RESET drops the table's TTL
setting and a lone SET creates it; any other step alters it, because the host
refuses a provider step that creates what another provider step drops (it runs
every creation of a phase before every drop), and the dependency orders a
pair. The declaration is held to the
rules of a creation, and a tier that moves rows needs `tiered_ttl`.

Each step
reads the table from the table service and the monitoring endpoint, refuses
before it writes when the table holds another TTL than the plan saw or when the
TTL to set reads a column that is absent, not first in the key or of a type its
mode does not read, runs its statement once through the query service and reads
the TTL back.

`internal/tablesource/testdata/corpus` holds the inputs and the answers of the
host's bundled decoder for them, and the differential test in
`internal/tablesource` requires the provider to mean the same as the host for
every input the host accepts, to refuse every input the host refuses, and to
name every difference in the table above. `testdata/oracle` explains how the
answers are regenerated against a host checkout.

## Offline column declarations

The `values.convert` service converts column properties between distinct typed
observed and desired models. It preserves family assignment, typed literal
defaults, sequence configuration, compression settings, and encoding order.
Identity, storage type, and nullability remain in the common column contract.
An omitted default requests the native no-default setting and stays omitted
during normalization; `dropDefault: true` expresses removal explicitly.
Unknown fields, unknown enums, duplicate keys, null settings, and overlapping
defaults are refused. Explicit sequence configuration retains optional values,
including `cycle: false`.

Sequence positions are mutable data. An observation containing `setVal` refuses
conversion rather than exporting a request to reset the sequence. Conversion
validates the source payload and its captured normalization, retains exact owner
identity and ordered records, and returns no prefix on failure or cancellation.
It opens no connection and establishes no inspection coverage.

The column-property models complement the common column service and preserve
native settings through conversion and JSON schema export.

## Row-table model conversion

The row-table model preserves ordered primary keys, TTL, storage pools, column
families, attributes, partitioning policy, bloom-filter settings, read replicas,
tiering, and temporary lifetime. Unknown settings, duplicate keys or families,
invalid TTL modes, and null settings are refused. Attribute values remain exact,
including empty strings. The row-table reader interprets an unspecified native
storage kind as row storage; column storage retains its separate model.

Access declarations keep the owner and direct grants separate from requirements
on effective permissions. Effective permissions include inherited access under
[YDB's permission model](https://ydb.tech/docs/en/reference/ydb-cli/commands/scheme-permissions).
A present access declaration specifies the complete direct-grant set and the
required effective-permission set. These requirements do not add direct grants. Conversion
neither infers an inheritance switch nor changes permissions. The provider
advertises table and access comparison and planning. Their supported changes
and refusals are described in the [table reference](table-creation-and-rebuild.md)
and [runtime reference](migration-state.md#common-objects).

Physical shard boundaries are runtime topology. A captured value containing them
refuses conversion rather than turning the current split points into creation
settings. A starting layout (`uniformPartitions`, or `partitionAtKeys` as lists
of source text values) is a creation setting of the desired model only: a read
finds none, and converting a desired table to the observed representation leaves
it out. Partitioning policy remains declarative. Statistics and transient
object identity cannot enter the property model. Conversion preserves a complete
mixed table/column batch, works after the session closes, and establishes no
inspection coverage.

JSON schema export includes table and column facets together with native objects
and their captured coverage. Unsupported input refuses the whole conversion.

## Offline index and stream declarations

The `values.convert` service also converts index, changefeed, and standalone
topic properties between separate typed desired and observed models. It works
without an open session, retains exact owner identity, and returns no prefix
when any value in a mixed batch fails. Conversion establishes no inspection
coverage. The index model advertises comparison and planning, for adding and
dropping global secondary indexes of an existing row table (see
[Index alteration](indexes-and-table-execution.md#index-alteration)), and so do the topic model (see
[Topics](streaming-and-workload-objects.md#topics)) and the changefeed model (see [Changefeeds](native-objects.md#changefeeds)).

Index declarations retain the native index kind, ordered index/data columns,
and kind-specific settings. Each declaration includes complete requirements on
the server-owned implementation tables: ordered columns and keys, column
defaults, storage, partitioning policy, and access constraints. These are
requirements on the index implementation, not independent tables to create.
Missing implementation tables or columns refuse conversion. Physical shard
boundaries also refuse conversion; current splits are not creation settings.
The observed index model canonicalizes unspecified implementation storage as
row storage and excludes internal sequence positions. This changes its definition
digest; this provider is pre-v1 and carries no reader for the previous shape.

Topic declarations preserve partitioning policy, retention, codecs, write/read
quotas, attributes, metrics settings, and consumer configuration. Consumer
settings retain read timestamps, availability periods, codec preferences,
attributes, and streaming-consumer presence. Access uses the same direct-grant
and effective-permission requirements as row tables. Runtime partition topology
and statistics cannot enter a declaration.

Changefeed declarations include the child topic's settings as well as mode,
format, virtual/resolved timestamps, attributes, AWS region, schema-change
reporting, and user SIDs. `disabled` and `initialScan` are independent settings.
An observation in `STATE_INITIAL_SCAN` converts to enabled intent with
`initialScan: true`. Converting that declaration back predicts
`STATE_ENABLED`; it does not prove that a server finished scanning. Scan
progress counters remain excluded.

Unknown fields/enums, runtime state in declarations, null settings, duplicate
names, and incomplete child descriptions are refused. Empty index/changefeed
sets keep their own desired model definitions. Indexes, implementation tables
and changefeeds normalize by name; consumers keep the order the declaration
lists them in, and column and key order is retained.

## Offline native-object declarations

View declarations retain query text and table-service attributes, including
comments. The view and table APIs must agree about access metadata. Conflicting
access or additional table settings refuse conversion rather than creating
competing declarations for one view. The table API returns disabled partitioning
switches for views on both tested YDB lines. This exact placeholder is excluded;
other partition settings are refused.

External-source declarations preserve source type, location, and exact native
property keys and values, including authentication references. External-table
declarations preserve the data-source path, location, content options, ordered
column names and types, nullability, and typed column settings. Conversion does
not contact an external service or resolve a secret.

Coordination conversion retains optional timing and mode settings together with
access requirements. Native unspecified settings remain omitted. A desired
`struct_name` cannot convert to an observation because it has no server
counterpart. A runtime-reserved coordination identity is not exportable as a
user declaration.

Column-storage declarations retain hash-column order, partition and data-channel
counts, local-index settings, and TTL tiers. Optional false values, disabled TTL,
and omitted TTL stay distinct. The same validation applies to both
representations; native IDs and shard addresses have no declaration field.

The native models use distinct desired definitions. This changes experimental
definition digests and the coordination change/operation contracts. This
provider is pre-v1 and has no reader for the previous shapes.

The live CI jobs build a pinned Ptah host in a separate checkout and run
`schema inspect --format json` with an explicitly trusted provider manifest.
They compare every exported model with SDK conversion and check that coverage
keeps every inspected model family. These tests exercise row tables, columns,
views, external objects, and coordination nodes. Local runs enable the same
checks with `PTAH_HOST_TEST_BIN` pointing to that host binary.

## Offline column-storage observations

The column-storage decoder accepts captured monitoring JSON together with an
independent table-service description. It checks table identity, ordered
columns and primary keys, primitive types, nullability, and reported hash keys.
It preserves hash-column order, partition and data-channel counts, local
bloom/ngram/min-max indexes, index storage settings, and TTL tiers. Omitted
index options remain absent. Explicit false values remain explicit.

Unknown schema fields, repeated fields or aliases, inconsistent descriptions,
unsupported type extensions, and configured column families are refused.
Native IDs, schema versions, and shard addresses are excluded from the model.
Local indexes are sorted by name; hash columns and TTL tiers retain order.

Fixture replay covers YDB 25.1 and 26.2 captures: monitoring pages copied from
the host, and pairs of a monitoring page and a table-service description read
from one live table each (`internal/columnstore/testdata/README.md`). A
subprocess test checks SDK normalization and conversion without opening a
database session, and live tests inspect column tables on both lines.
