---
title: YDB indexes and table execution
description: Index alteration, vector and full-text indexes, execution, and table-property comparison.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How does Ptah plan and execute YDB index and table changes?"
goal: "Find YDB index operations and table execution rules."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 15
---

# YDB indexes and table execution

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Index alteration

The service adds and drops global secondary indexes of an existing row table.
Each index is one native `AlterTable` step: `row-table-add-index/N` reads the
table and creates one index object, `row-table-drop-index/N` reads the table and
drops one. The index object is a host subject of kind `index`, named by the
index and parented by the table, so two index steps of one table never share an
effect. An addition is `ADD INDEX name GLOBAL [SYNC|ASYNC] [UNIQUE] ON (keys)
[COVER (columns)]`; a removal is `DROP INDEX name`. Impact is additive for an
addition and destructive for a removal.

A host that lists `planning.index-renames` pairs an index the table holds with
one the declaration states under another name and one definition, and sends
the pair as one rename step. The service claims it with
`row-table-rename-index/N`, a native `AlterTable` call with `rename_indexes`,
whose text form is `RENAME INDEX old TO new`. The step reads the table, drops
the old name and creates the new one. The index keeps its kind, its columns
and its partitioning settings, and is not built again (measured on 25.1.4.7
and 26.2.1.14). Impact is
behavioral: a query that names the old name stops reading through the index.
The host moves the index's comment to the new name with its own steps.

Execution refuses before it writes when the table no longer holds the index as
planned or holds an index under the new name. After the call it requires the
old name to be gone and the new one to hold the index as it was, settings
included. YDB renames an index only with `EnableMoveIndex` on, so a rename
step on a session without `index_rename` is refused when it is planned. A session
reports the capability only to a host that lists the feature; any other host
plans a rename as a removal and an addition.

The common indexes a source declares, such as a `ptah:schema:index` directive,
are compared by the host. Inspection reports a row table's global indexes as
its common indexes: a synchronous one, unique or not, with no method, and an
asynchronous one with the method `async`. For a host that lists
`schema.index-methods`, table preparation states the kind of each declared
index in the same spelling, reading its type with the host's embedded engine's
spellings: none, `BTREE`, `GLOBAL`, `SYNC` and `GLOBAL SYNC` are synchronous,
and `ASYNC` and `GLOBAL ASYNC` are asynchronous, in any case and with `_` for a
space.

The host compares the two spellings, so a declaration that changes only
an index's kind is a removal and an addition of the index under its name, which
this service carries out as the two steps above, the removal first. A host
without the feature compares no kind, and such a change plans nothing.

### Index settings from a source

A Go `ptah:schema:index` directive, the index entry of a YAML table and the
`index` block of an HCL document state a global index's partitioning and read
replicas with the attributes the host's embedded engine reads there:
`auto_partitioning_by_size`, `auto_partitioning_partition_size_mb`,
`auto_partitioning_by_load`, `auto_partitioning_min_partitions_count`,
`auto_partitioning_max_partitions_count` and `read_replicas_settings`, with the
spellings a table directive takes. They decode into the index's own facet
(`index-properties`), owned by the index, and a value an index cannot take,
such as `auto_partitioning_by_load="true"` or a count of zero, is refused where
it was written.

For a host that lists `schema.observed-index-facets` (contract
25.21, ADR 0068 in Ptah), inspection reports the same facet on each common
index, with every setting the server describes on it, defaults included, so
the host compares one owner's facet on both sides. A host that does not list
the feature reads no index's settings, and the provider refuses a declared
setting from it, since the comparison could not see what the index holds.

```go
//ptah:schema:index name="by_body" fields="body" auto_partitioning_by_load="ENABLED" auto_partitioning_min_partitions_count="2"
```

- A setting the source leaves out keeps what the index holds, as the host's
  embedded engine compares it. A differing setting is a
  `github.com/stokaro/ptah-ydb-plugin/row-table-properties-change` on the index,
  planned as the `row-table-index-settings/N` step above.
- A table the plan creates is created with its indexes, and each index whose
  facet declares settings then takes them in a `row-table-index-settings/N`
  step after the creation, since no call that creates an index takes them. An
  index added to a table that exists takes them after its addition.
- An index the plan renames in place keeps its settings: the host compares a
  renamed index's facet under its new name, and a changed setting is a
  settings step after the rename, as the host's embedded engine writes `RENAME
  INDEX` and then `ALTER INDEX ... SET`. An index the plan drops is left alone.
- Settings declared on an index that is not a global sync, async or unique
  index, or on one the table neither holds nor gains, are refused.
- A table rebuild gives each index of the new table its declared settings, and
  what the old index holds for every other (see
  [Native table rebuild](table-creation-and-rebuild.md#native-table-rebuild)).
- `migrations generate` writes the reverse of a change of an index's settings
  into the down file: every setting the index held, restored in place (see
  [Reverse migrations](migration-state.md#reverse-migrations)).
- An export writes an index's facet as the settings that differ from the ones
  YDB gives a new index, as the host's embedded engine writes them, so `ptah
  introspect` writes `auto_partitioning_by_load="ENABLED"
  auto_partitioning_min_partitions_count="2"` on the directive, and the export
  applies back with nothing planned.

The declaration is the index set model (`table-indexes`, desired side) as a
facet of the table, beside the table properties. It is managed when the
declaration states it: an index the table holds and the set leaves out is
dropped, and a declaration with no index set leaves the indexes alone. The
comparison reads the specification of each declared index: the name, the kind,
the ordered key columns and the ordered covered columns. The declared
`implementation` requirements on the server-owned index table are part of the
model and have to be valid, but they are not compared and not sent: YDB builds
that table itself.

- YDB cannot alter an index in place. An index the table holds under a declared
  name with another kind, other key columns, other covered columns or another
  key order is a change the planner refuses with a diagnostic that names both
  definitions. It does not plan a drop followed by an addition, because that
  rebuilds the index. Declare the removal and the addition in separate plans if
  a rebuild is wanted.
- The rules above are those of global sync, async and unique indexes. A vector
  or full-text index follows its own (see
  [Vector and full-text indexes](#vector-and-full-text-indexes)). An index to
  add that states its partitioning or read replicas is added without them,
  since no call that adds an index takes them, and a
  `row-table-index-settings/N` step after the addition gives it the declared
  ones over the settings YDB gives a new index (automatic partitioning by size
  at 2048 MB, none by load, at least one partition, no read replicas; measured
  on 25.1.4.7 and 26.2.1.14), as the host's embedded engine writes `ALTER INDEX
  ... SET` after `ADD INDEX`. Execution of that step requires the new index to
  hold exactly those settings.
- A unique index added to a table that exists needs
  `unique_index_on_existing_table`. YDB 25.1 and 25.2 never add one, and later
  lines keep it behind `EnableAddUniqueIndex`, off by default; without it the
  server refuses the call (`Adding a unique index to an existing table is
  disabled`) with an outcome the provider cannot confirm. So a session without
  the capability refuses such an addition when it is planned, in the host's
  embedded engine's words: `adding unique index "<name>" to table
  "<table>", which exists already, which requires target capability
  unique_index_on_existing_table, unavailable on this ydb target`. A table
  the plan creates takes its unique indexes with it and needs nothing.
- An index reads columns the declaration states. A declared index that reads a
  column the declaration does not state is refused, and so is a kept index whose
  column the declaration drops.
- Ordering against column steps of the same table, whoever carries them out: an
  index is added after the addition of every column it reads, and dropped before
  the removal of every column it reads or covers. A column removal step that does
  not name its column is assumed to remove any column. Execution reads the
  indexes of the table before dropping a column as well, so a column an index
  still reads is refused before the call.
- The partitioning and the read replicas of an index the table holds, stated in
  the `settings` of its global kind, change in place with one step,
  `row-table-index-settings/N`, that reads the table and alters the index. It
  runs `ALTER TABLE ... ALTER INDEX ... SET (...)` through the query service, as
  the host's embedded engine writes it, because the native AlterTable call has
  no field for an index's settings. The rules are the table's: a setting the
  declaration leaves out keeps what the index holds, the statement names every
  partitioning setting the index ends with when the partitioning changes, and
  replicas declared as none are written as `PER_AZ:0`. A starting layout and
  partition columns are refused by name. The server describes an index's
  settings on the index itself (measured on 25.1.4.7 and 26.2.1.14), and the
  comparison reads them there. Impact is behavioral.
- YDB builds a new index in the background. The provider waits for the index to
  read back as ready, bounded by ten minutes, before it confirms the step.
  Inspection refuses an index that is not ready, so a table with an index in
  the build is never compared.

## Vector and full-text indexes

A vector index (`vector_kmeans_tree`) and a full-text index (`fulltext_plain`,
`fulltext_relevance`) are entries of the native index set, with their settings,
and the comparison, the planner and execution follow the rules of the host's
embedded engine for them:

- An index the set states and a table that exists does not hold is added with
  one step, `row-table-add-index/N`, of its own operation kind,
  `row-table-native-index-operation`. The native `AlterTable` call of the
  pinned bindings carries neither kind, so execution runs YQL's `ALTER TABLE ...
  ADD INDEX` through the query service, as the host's embedded engine writes
  it: `INDEX name GLOBAL USING vector_kmeans_tree ON (prefix, vector) [COVER
  (columns)] WITH (distance=cosine, vector_type=float, vector_dimension=3,
  levels=1, clusters=2)`, the metric spelled `similarity=` where the index
  names a similarity, and `INDEX name GLOBAL USING fulltext_relevance ON
  (column) WITH (...)` with the analyzers in name order and the language
  quoted. The TEXT form is the same statement.
- An index the table holds and the set leaves out is dropped by the native
  `AlterTable` call, `row-table-drop-index/N`.
- YDB changes nothing of a vector or full-text index in place (`ALTER INDEX ...
  SET (levels=2)` answers `Unknown table setting: levels`). An index whose
  columns, kind or settings differ is dropped and added again under its name,
  the removal first, and a renamed index is dropped under its old name and added
  under its new one. The table is not rebuilt. A global index under the name of
  a vector or full-text one, or the other way round, is refused.
- The comparison reads an index's name, its ordered key and covered columns,
  its kind and its vector or full-text settings, exactly; a full-text filter
  switched off is read as one the index does not name, because YDB applies no
  filter it is not told to. The settings of the tables that implement the index
  are not compared: the server reports its defaults for them on every index, and
  YDB changes none of them on a vector index that exists. An addition that
  states other settings for them than YDB gives a new index is refused.
- A vector index names one metric, a vector type, a dimension from 1 to 16384,
  levels from 1 to 16 and clusters from 2 to 2048, with clusters to the power
  of levels at most 2^30, the limits 25.3 and later hold every index to; 25.1
  and 25.2 check none of them, and the planner holds every line to them, as the
  host's embedded engine does. Its last key column is the vector, a `String`;
  the others are its prefix. Overlap settings are refused by name.
- A full-text index reads one `String` or `Utf8` column of a table whose key is
  one `Uint64` column (26.2 answers `Fulltext index requires primary key column
  ... to be of type 'Uint64'`), analyzed by the one column entry of its
  settings, with a tokenizer. An n-gram filter names both its lengths, a
  language goes with the snowball filter, and the snowball filter takes no
  n-gram filter, as 26.2 answers.
- A target without `vector_indexes` refuses a vector index before a statement
  is sent, one without `vector_bit_type` an index over bit vectors, and one
  without `full_text_indexes` a full-text index (see
  [Feature flags](connections-and-execution.md#feature-flags)). A server answers each with `Vector index
  support is disabled`, `bit vector type is not supported` (25.1, over rows),
  `FULLTEXT_RELEVANCE index subtype is not supported` (25.1) or `Fulltext index
  support is disabled` (26.2 with `EnableFulltextIndex` off).
- Execution reads the table, requires the index to be absent and its columns
  to exist, runs the statement once, reads the table back until the index is
  ready, and requires it to hold the requested definition and everything else
  to be unchanged.
- A table rebuild of a table that declares or holds a vector or full-text index
  is refused: the new table is created with its global indexes alone.

### Index declarations of the provider's types

The host's frontends build a common index from every index declaration. For a
host that lists `source.index-kinds` (contract 25.24), the provider claims the
index types it models outside the common model, and the host sends a
declaration of one of them whole, as one fragment, and builds no common index
for it: `vector_kmeans_tree`, `fulltext_plain` and `fulltext_relevance`, which
declare the table's native index set, and `bloom_filter`, `bloom_ngram_filter`
and `min_max`, which declare a column table's local indexes in its storage. A
type may be written with a space for each underscore and with `GLOBAL USING` or
`LOCAL USING` in front, in any case.

The fragment carries the index's columns,
the columns it covers and a pgvector operator class (`ops`, which names a vector
index's metric as the host's embedded engine reads it: `vector_cosine_ops` is
`distance=cosine`, `vector_ip_ops` `similarity=inner_product`), and the
settings the index's own directive, entry or block writes:

```go
//ptah:schema:index name="docs_emb" fields="gen,emb" include="body" type="vector_kmeans_tree" distance="cosine" vector_type="float" vector_dimension="3" levels="1" clusters="2"
//ptah:schema:index name="ft" fields="body" type="fulltext_relevance" tokenizer="standard" use_filter_lowercase="true"
//ptah:schema:index name="bf" fields="body" type="bloom_filter" false_positive_probability="0.01"
```

A YAML index entry takes the same keys, and an HCL `index` block the same
attributes, a count as a number. Every vector or full-text declaration of a
table makes one native index set, and every local one joins the column
storage its table directive or entry states. Refused by name: a unique index,
an index comment, a setting of another kind of index, an operator class on an
index other than a vector one, a local index of a row table, a vector or
full-text index of a column table, and an index declared twice. A setting the
index cannot take is refused with the host's embedded engine's words, at the
source for its grammar and at planning for its limits.

A Go or YAML source of such a host states the native index set of every table
it declares, so a table that writes none of these indexes is declared to have
none, and one the table holds is dropped; a column table's local indexes are
managed likewise. Without the feature a source cannot declare them, and a table
keeps the ones it holds. An export writes each index back as a declaration of
its type, spelled as the host's embedded engine wrote it in HCL: `type =
"GLOBAL USING vector_kmeans_tree"` with the columns, the covered columns and
the settings, and a local index's method alone, `type = "bloom_filter"`. The
embedded engine wrote no full-text or local setting in HCL, and refused the
full-text document it wrote; the provider writes and reads the full-text
settings there. HCL loses a column table's local indexes with its storage.

## Execution

Execution reads the table first. A family operation requires every touched
family and moved column to match the plan, and every family to add to be absent.
An addition requires the column to be absent. A removal requires the column to
exist and not to be a key column, an index column or stored column, the TTL
column or a partitioning column; the server refuses each of those only after the
call, which would leave the outcome uncertain, so the provider reads them first.
A TTL operation requires the table to hold exactly the TTL the plan captured, or
none, and a TTL to set must read a column the table holds with a type its mode
reads.

A settings operation requires the table to hold exactly the
partitioning settings, the key bloom filter and the read replicas the plan
captured for each group it changes. An index addition requires the index
to be absent and to read columns the table holds, and an index removal requires
the table to hold the index as the plan captured it. A change of an index's
settings requires the table to hold the index as captured, settings included. A table that changed since
planning is refused before the call.

After the call the provider reads the table back and checks that the requested
change holds and that nothing else moved: other families and columns, column
names and types, their order, the key, and the TTL. The TTL must read back as
the one the request sets, run interval included, or as none after a removal, and
as before for every other operation. After an index operation the added index
must be ready and hold the requested kind and columns, a dropped index must be
gone, and every other index must read back unchanged.

After a settings operation the table
must hold every partitioning setting the request names and the partition columns
it held; the settings the server derives from them, such as the partition size
it no longer reports once partitioning by size is off, are not compared. The
key bloom filter and the read replicas must read back as the ones set, or as
they were, and a request that names no partitioning setting leaves the
partitioning as it was. After a change of an index's settings the index must
keep its definition and be ready, and hold the settings by the same rules,
while every other index, every column and the key read back unchanged. An added column must read back with its
type, nullable and in the default family.

A failed reply or unconfirmed
read remains UNKNOWN and is never retried. As for creation, these checks do not
establish exclusive ownership against another schema writer.

Live tests alter a table on YDB 25.1.4.7 and 26.2.1.14: families and column
placements; adding columns, placing one in a family and dropping others in a
single plan; changing each partitioning setting on its own and together,
including switching partitioning by size off and on again; setting, changing, moving and removing a TTL, including the move of
a TTL to an added column in the same plan that drops the column it read; and the
refusal of an indexed column, of a TTL column the server would refuse, and of a
stale alteration before the call; and adding and dropping sync, async, covering
and unique indexes, with the refusal of a changed index definition.

They set,
change and remove the read replicas and the key bloom filter of a table, alone
and with its partitioning, and the partitioning and read replicas of sync,
async and unique indexes, including switching partitioning by size off and on
again with a minimum the declaration leaves out, and refuse a stale settings
plan before the call. The TEXT form of each change is run on a twin table and
reads back the same. They plan again from the read back to show the declaration no
longer changes anything, and check that rows survive.

## Table property comparison

The offline `models.compare` service `ydb-row-table-properties` compares the
table and column properties of a row table that exists on both sides. It reports
one `github.com/stokaro/ptah-ydb-plugin/row-table-properties-change` for a table whose TTL, partitioning settings, read replicas,
key bloom filter or column families differ from the declaration, one for each column that sits in another
family, and one for a table whose index set differs from the declared one: an
index added, removed or changed, or an index whose partitioning or read
replicas differ from the ones it declares. An index change carries both complete index
sets and names them with a `facet` key, so a table can have a property change
and an index change at once.

A changefeed set that differs is reported the same
way, with the changefeed set as its facet; a source that describes the
changefeeds of every table it declares states a table with none as one without
changefeeds (see [Changefeeds](native-objects.md#changefeeds)). The comparison applies the rules of the alteration planner, so a
difference it reports is one the planner acts on, and a table that differs only
in its TTL, its settings or its families still reaches the planner. A change carries both
complete facets, replaces no owner, removes no state and leaves access
unchanged. A facet present on one side only states nothing to compare; the
planner decides what the missing read means. The planner accepts exactly these
changes, accounts for each one with the steps of its table alteration, and
refuses every other change kind.

The host names a table and its columns without a database, because they live in
the session's, while the provider's inspection names that database in the owner
of every facet it reads. The comparison and the planner read a facet whose
subject names no database without its owner's, so both sides of one table name
one owner. A facet whose owner is another table than its subject names is
refused.
