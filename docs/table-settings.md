---
title: YDB table setting changes
description: Column families, column changes, TTL, partitioning, replicas, and bloom filters.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "Which YDB table settings can Ptah change in place?"
goal: "Find YDB table setting changes and limits."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 14
---

# YDB table setting changes

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Native table alteration

The same planning service (`ydb-row-table-create`) accepts captured ALTER
lifecycles for row tables. It owns five facets of an altered table: its column
families, the addition and removal of its columns, its TTL, its settings (the
automatic partitioning, the read replicas and the key bloom filter), and its
global secondary indexes with their partitioning and read replicas. Each
becomes native AlterTable calls, except the settings of an index, which the
native call cannot state and YQL's `ALTER INDEX` changes. Everything else about an altered table stays with the host:
other table settings, and any column step the native call cannot state. The
service also plans the changefeeds of a new or an altered table (see
[Changefeeds](native-objects.md#changefeeds)).

### Column families

The service reads the declared and the observed table and column properties,
and plans one call that adds families, changes the settings the declaration
states, and moves columns between families. The rules follow the host's family
rules:

- A setting the declaration leaves out is kept as the table holds it. A cluster
  profile gives new tables settings of their own, so the table is not reset.
  A declared `COMPRESSION_NONE` matches a family that reads back unspecified.
- A cache mode the declaration states is set where the family holds another
  one or none. A family that never had one reads back with none, so a declared
  `regular` is set once and then holds, as the host's embedded engine sets it.
  The request carries it as field 6 of the family, as a creation does (see
  [Native row-table creation](table-creation-and-rebuild.md#native-row-table-creation)). A declared cache
  mode, `regular` included, is refused on a target without
  `column_family_cache_mode`, with the flag's name, whatever the table holds:
  the table service of 25.1.4.7 would alter the table without it.
- A family only the table holds stays. No family is dropped, because YDB has no
  way to drop one.
- A column sits where the declaration places it. A column property record
  names its family, and an empty family in a record means the default family.
  A source that describes the column properties of every column it declares (a
  Go, YAML or YQL source) places a column it names in no family in the default
  family, so a column the table holds elsewhere moves back, as the host's
  embedded engine moves it. A source that could not describe them, such as
  HCL, says nothing of where a column sits, and the column keeps its family. A
  rebuild places the columns of the new table by the same rule.
- A key column stays in the default family. A column that names a family
  neither the declaration nor the table has is refused. So is a `keep_in_memory`
  that only one side sets, and a declaration that states families when the
  table's current families were not captured.
- A column the plan removes is never named, so it is never moved.

A table whose families already match yields no family step and receipts that
say so.

### Column addition and removal

The host plans one common step for each column it adds or drops. The service
replaces such a step with a native step of its own, so the column steps and the
family step form one graph with one owner.

- A step is replaced when its footprint is a read of the table and the creation
  or removal of one column of that table. A step with any other footprint stays
  with the host.
- An added column must be nullable, with no default, key membership, unique,
  check, reference, generated, identity, collation or comment, and no column
  property but its family. YDB refuses a NOT NULL column on an existing table
  (measured on 25.1.4.7 and 26.2.1.14), and accepts a default on 26.2 only. The
  host keeps the step for any column outside this subset.
- An added column starts in the default family. A column property record that
  names another family moves it in a separate family step, which runs after the
  addition.
- A dropped column must not be one the declaration states. The service refuses
  a request in which the host drops a declared column, adds an undeclared one,
  or changes one column twice.
- Each replaced step becomes one native call, named `row-table-add-column/N` or
  `row-table-drop-column/N`. The family step is `row-table-alter/N`. Ordering
  among the host's steps is kept, and the family step runs after the addition of
  every column it moves and before every removal. Both ends of those edges are
  steps of this provider. Where a step stays with the host, the edge names the
  host step.

### TTL

The service sets, changes and removes the TTL of an existing row table with one
native `AlterTable` step, `row-table-ttl/N`. A step carries the whole TTL the
table holds afterward (`set_ttl_settings`) or removes it
(`drop_ttl_settings`), and also the TTL the table held when the plan was made.

- The TTL is managed when the declaration states table properties for this
  target, or when the desired coverage establishes the table properties of the
  table: complete knowledge of the kind, or an absence or a request for
  defaults recorded for the table. A table the declaration leaves without a
  `ttlSettings` then requests none, and a TTL the table holds is removed. Unknown
  or unrepresentable desired knowledge with no table properties leaves the TTL
  alone. A declaration that states table properties while the table's current
  TTL was not captured is refused; a table managed by its coverage alone, whose
  current properties were not captured, is left alone.
- The declared and the held TTL are the same when the mode, the column, the
  unit and the seconds are the same. A `runIntervalSeconds` the declaration
  leaves out is not compared. A step that replaces a TTL carries the run
  interval the table held unless the declaration states one, because the call
  resets an interval it does not carry (measured on 25.1.4.7 and 26.2.1.14).
  The server refuses a run interval below its configured limit, 900 seconds on
  the test servers, after the call; the plan does not check it.
- The column must be declared, with a type the mode reads: a Date, Datetime,
  Timestamp, Date32, Datetime64 or Timestamp64 column for a date TTL, and a
  Uint32, Uint64 or DyNumber column with a unit for an integer TTL. The server
  refuses each other combination only after the call, so the plan refuses it.
  A key column is allowed.
- The TTL step runs after the addition of the column it reads, and before the
  removal of the column the table's TTL reads now, whoever carries those steps
  out. The server refuses a TTL on a column the table does not hold, and refuses
  to drop the column a TTL reads. The family step and the TTL step do not
  depend on each other.
- Impact is behavioral for a set and for a removal: a TTL decides which rows
  YDB deletes, and restoring a prior TTL does not recover deleted rows.
- The host capabilities `RowDeletionPolicy` and `RowDeletionPolicyEpochColumn`
  are not session capabilities of this provider. The host keeps refusing a TTL
  its target cannot write.

### Partitioning settings, read replicas and key bloom filter

The service changes the automatic partitioning settings, the read replicas and
the key bloom filter of an existing row table with one native `AlterTable`
step, `row-table-partitioning/N`, as the host's embedded engine changes them
with one `ALTER TABLE ... SET`. The step carries only the settings that change,
and also the value the table held for each group it changes: the whole
partitioning, the filter, or the replicas. The settings are `AUTO_PARTITIONING_BY_SIZE`,
`AUTO_PARTITIONING_BY_LOAD`, `AUTO_PARTITIONING_PARTITION_SIZE_MB`,
`AUTO_PARTITIONING_MIN_PARTITIONS_COUNT` and
`AUTO_PARTITIONING_MAX_PARTITIONS_COUNT`, which the declaration spells
`partitioningBySize`, `partitioningByLoad`, `partitionSizeMb`,
`minPartitionsCount` and `maxPartitionsCount`.

- The settings are managed when the declaration states table properties for this
  target, and only the settings it states are compared. A setting the
  declaration leaves out stays as the table holds it, so the defaults a table
  reports for the settings the declaration omits never count as a difference.
  The native call has no reset, so removing a setting from the declaration does
  not return it to its default.
- Partition columns are set when the table is created. A declaration that names
  other columns than the table holds is refused; the same columns are no change.
- A starting layout is set when the table is created, and YDB keeps no record of
  it except the minimum partition count it sets from it: the count of the layout,
  unless the declaration states a minimum of its own. That minimum is the only
  witness an existing table has, so a declaration whose layout sets the minimum
  and whose minimum differs from the table's is a change only a
  [rebuild](table-creation-and-rebuild.md#native-table-rebuild) makes. The new table is created with the
  layout and the held value of every other setting, the minimum aside, which
  the layout sets. Without a rebuild asked for, the planner refuses it with
  `UNIFORM_PARTITIONS alter is not supported` (or the `PARTITION_AT_KEYS`
  spelling), the two minimums, and the request that asks for a rebuild, such
  as `--allow-table-rebuild`. A layout beside a declared minimum leaves no
  witness and is not compared. The comparison reports the layout as a
  difference, so that the plan reaches the planner.
- A minimum count above a nonzero maximum is refused, checked against the value
  the table holds for a bound the declaration leaves out.
- A partition size declared without `partitioningBySize` turns partitioning by
  size on, as the server does when a size is set and as the host's embedded
  engine reads the declaration (measured on 25.1.4.7 and 26.2.1.14 for a table
  and an index that did not split by size). A size beside a declared
  `partitioningBySize` of `DISABLED` is refused.
- When the step switches partitioning by size or by load on, it names every
  setting the table keeps: the ones the declaration states and the held value
  of the others. Switching either on resets the minimum count the call leaves
  out to 1, and switching partitioning by size on resets the size too
  (measured on 26.2.1.14 through the table service for partitioning by load),
  so a minimum nobody declared would otherwise drop to 1. The size the table
  takes is 2048 MB unless the declaration states one, and the TEXT form names
  it. An index's settings follow the same rule.
- `keyBloomFilter` takes `ENABLED` or `DISABLED`. A table that reports the filter
  as unspecified keeps none, so a declared `DISABLED` matches it (measured on
  25.1.4.7 and 26.2.1.14 by the host's embedded engine).
- `readReplicasSettings` takes `perAzReadReplicasCount` or
  `anyAzReadReplicasCount`. A count of zero in either mode is no replicas, and a
  table that holds some loses them through `PER_AZ:0`, since YDB has no reset
  for them. Replicas the declaration leaves out stay.
- Setting one of the three groups resets nothing of the others, so the step
  names a filter or replicas only where they change. Execution confirms that
  the groups the step does not name read back as they were.
- Impact is behavioral: the settings decide when YDB splits and merges
  partitions and where it serves reads. The step reads the table and alters its
  settings only, and does not depend on the family, column or TTL steps.
