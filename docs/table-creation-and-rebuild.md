---
title: YDB table creation and rebuild
description: Column semantics, schema validation, table creation, removal, and rebuild.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How does the provider create and rebuild YDB tables?"
goal: "Find YDB table creation, removal, and rebuild rules."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 13
---

# YDB table creation and rebuild

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Common column semantics

Inspection requires the negotiated `schema.observed-columns` feature. Row and
column tables report complete common column facts, including decimal precision
and scale, literal default presence, and serial start and increment settings.
The last sequence restart remains observed evidence; it is not a request to
restart the sequence during source conversion. Native column facets retain the
exact settings needed to check execution before-state. Unknown or contradictory
native metadata refuses the complete inspection reply.

Column default changes use captured native before-state and typed values. The
provider plans one `ALTER COLUMN SET DEFAULT` or `DROP DEFAULT`, outside a
transaction, and verifies the column before and after execution. An empty string
is a present literal. A stale before-state refuses execution; an unconfirmed
write has an unknown outcome and is not retried. The session reports
`alter_column_default` from the release line and `EnableSetDropDefaultValue`.

A default change, a column made nullable, a column removal and a sequence
change each hold the native column they change. The side the host builds from a
schema file for a diff of two files states its column facets completely and
carries one only for a column whose declaration states a native setting, so a
column without one is held as the common column says, its common default read
back as the native literal. A diff of two files plans these changes offline, as
Ptah's built-in YDB engine did. A side that does not state its column facets
refuses them.

The parent planner derives column additions and removals from the complete
captured table. Nullable additions without defaults use native `AlterTable`.
Additions with typed literal defaults use one pinned YQL `ADD COLUMN`; existing
rows receive the default, including an empty string for a `NOT NULL` column.
They require `add_column_with_default`, established from the release line and
`EnableAddColumsWithDefaults`. A `NOT NULL` addition without a default is refused.
A removal checks its captured column definition and refuses key, index, TTL, or
partitioning dependencies before dispatch. Each operation verifies the remaining
columns and table settings after its single write. The graph orders additions
before the default and sequence changes, and removals after them.

The table's
family, TTL, partitioning, index and changefeed steps are outside that
sequence. Each carries its own edges to the column steps it depends on, and
otherwise runs where the host places it: the host drops an index before it
adds a column.

The negotiated `schema.columns` service compares the exact columns the host
pairs inside their full captured tables. It reports type and default
equivalence, effective nullability, and sequence start and increment differences.
Primary and serial columns have effective `NOT NULL`; the original declaration
is retained. Readback distinguishes absent defaults, empty literal values and
expressions. Creation projection reports every effective column in declaration
order, including its typed literal default and comment.

Table compilation maps supported SQL aliases using the selected target's
capabilities. Row-table literal defaults compile into typed native values;
expressions are refused. A default has one source declaration, either a common
column field or native column settings. Wide date/time types and parameterized
Decimal follow the cluster flags; small integer and document defaults follow the
measured release line. A column table accepts no defaults or serial columns.
Serial columns are created with their sequences and the sequence of an existing
Serial changes in place; see [Serial columns](#serial-columns).

## Offline table preparation

The experimental `schema.prepare-tables` service derives column key membership
from a complete captured row-table declaration. It uses the same native compiler
as CREATE, including ordered composite keys and native properties. Source values,
observations, and explicit existence knowledge remain unchanged. The service
opens no session and refuses the whole batch when any declaration is unsupported.
It does not predict server defaults or enable the host's common SQL table planner.

## Offline CREATE prediction

The experimental `schema.project-table-creations` service predicts column key
membership and effective desired table properties from complete declarations.
It uses the native CREATE compiler, including composite key order and authored
settings. Computed values remain on the captured table owner. Explicitly excluded
table properties stay excluded, and source declarations remain unchanged.
Offline representation conversion preserves an absent database binding. Live
capture still requires the database that supplied the observation.

Unspecified server defaults remain unspecified. The service returns no observed
overrides and makes no claim that a table exists or a migration ran. Both process
boundaries validate complete batches, identities, exclusions, and key membership.
Live tests feed the prediction into native planning, execute CREATE, and inspect
the resulting table on both qualified server lines.

## Offline schema validation

The experimental `schema.validate` service checks the captured desired schema
before the host plans. The host asks for it and shows the reply to
the user; without the service `ptah schema plan` stops with `target "ydb" has no
schema validation service`. The service reads only the request. It opens no
session and loads no source file.

Each table declaration is compiled with the CREATE rules, so validation refuses
what planning would refuse: a missing or unknown key column, a type with no YDB
counterpart, a table without columns, populated declaration fields and facets
the provider does not carry (including common indexes), and invalid names. Access
requirements and temporary tables are valid declarations that only CREATE
refuses, so validation accepts them. A setting the target takes only with a
capability is refused where the request's capabilities lack it: the settings of
a Serial's sequence, and a family's cache mode, which the host's bundled
validation refuses for `--server-version` 25.1 and 25.3 and accepts from 25.4.

Table paths that coincide or nest are refused
across the schema, because a YDB table holds no children. Directory names are
checked as path elements. The native models in the whole-schema capture must
normalize.

A refusal of the declaration is a diagnostic in a completed reply. Cancellation,
an identifier context the provider cannot interpret and a capture whose receipt
differs from the provider's own normalization are errors and carry no
diagnostics. Diagnostics are sorted, so the same schema gives the same reply in
any capture order. A key, type or unsupported-field refusal is
`UNSUPPORTED_FEATURE`, as in the host's bundled YDB validation; other refusals
are `INVALID_SCHEMA`. A standalone enum, and a directory comment, character set or
collation, are reported as unsupported and `OMITTED_DECLARATION` only when the
request asks that nothing be skipped, which is also the host's behavior.

So is
what an offline render of a table's creation goes without (see [Offline schema
rendering](yql-reference.md#offline-schema-rendering)), in the host's bundled validation's
words: `ydb: column "app.accounts.email": type modifier=VARCHAR(255): length 255
would be skipped` and `identity start=100 would be skipped`.

`internal/schemavalidation/testdata/corpus` holds declarations and what the host's
bundled validation answered, recorded by `testdata/oracle` (the program header says
how to run it against a host checkout). Rows named `refused-` agree with the host on
code, `deferred-` rows are ones the host accepts and this provider refuses, and the
others agree on acceptance and on what would be skipped. The provider imports no host
code. A subprocess test runs the service through the SDK client, and a host contract
test (`PTAH_HOST_TEST_BIN`) checks that the host reaches the service and shows its
refusal.

## Native row-table creation

The SDK planner accepts captured CREATE lifecycles for row tables. It retains
ordered composite primary keys, native column types and nullability, TTL,
storage settings, column families, attributes, partitioning settings, starting
layouts, bloom filters, read replicas, and tiering. Column families and typed
literal defaults come from native column facets. Missing creation settings
request server defaults. The common and native primary key declarations must
agree on order. A declaration that orders no key, such as a YAML table with
two columns marked `primary: true`, is keyed by the columns it marks primary in
the order it declares them, as the host's embedded engine keys it; one that
marks none is refused.

A family's cache mode travels as field 6 of the request's family, its upstream
wire number, which the pinned Go bindings do not declare and keep as an unknown
field; the operation payload writes it as the family's `cacheMode`, its
upstream JSON name. The table service of 25.1.4.7 does not know the field and
creates the table without it, so a creation that states one is refused where
the session reports no `column_family_cache_mode`, and the creation is
confirmed by the cache mode the table reads back with.

A starting layout becomes the `partitions` of the CreateTable request, checked
against the key columns: `uniform_partitions` needs a Uint32 or Uint64 first key
column; a split point holds at most one value per key column, each value a whole
number in the range of its integer column or a string for a Utf8 or String
column, and no other key type takes one. Each value travels as an optional
element of a tuple over the leading key columns. Split points out of order are
left to YDB, which refuses them. The creation is confirmed by the minimum
partition count YDB sets from the layout, unless the declaration states a
minimum of its own. The operation payload is checked again against the same
rules, so a forged request cannot state a layout the rules refuse.

The planner can replace a complete common CREATE TABLE step in the shared graph.
Its replacement carries table and physical-path effects and forbids SQL
transactions. A refusal returns no successful planning prefix. Rendering emits
a native operation envelope and validates its safety metadata again.

Execution establishes absence through the complete listing of the parent
directory, or through the proof that the directory does not exist, which
CreateTable then creates with the table. An existing path is refused,
including an existing table with the same schema. The provider binds the session and absolute path, checks
its guarding leases, sends CreateTable once, and reads the result back. It
confirms columns, key order, requested settings, and the absence of unexpected
indexes or changefeeds. A failed reply or unconfirmed result remains UNKNOWN.
The native API provides no atomic compare-and-set; these checks do not establish
exclusive ownership against another schema writer.

A table is created with the global indexes it declares, synchronous or
asynchronous, unique or not, with their key and covered columns, in the one
CreateTable request, as the host's embedded engine writes them inside its
CREATE TABLE. That is the only way to give a new table a unique index on a
release that refuses one on an existing table. What a YDB index has no clause
for is refused in the embedded engine's words: an expression, an order, a
prefix, a NULLS order or an operator class on a key column, an index on the
table's key, a covered key column. A vector, full-text or local index is
refused as a common index, since the pinned table protocol builds none of them
with a table.

An index names its table as the host names it: by its name, by
its directory and name, or, where a part holds a dot, by the quoted reference
`"app.records"` for the root table app.records and `app."v.1"` for the table
v.1 of the directory app. Execution confirms each index by reading it back. In
the TEXT form each index is an INDEX clause of the CREATE TABLE.

YDB has no UNIQUE constraint: it refuses one as a parse error and holds the
same rows with a global unique index, a second row with the same value refused
and rows whose value is NULL not. A session reports `unique_constraints` as
`false`, and a host that reads it so turns a declared UNIQUE constraint, and a
column's UNIQUE, into that index before it compares and plans, under the name
its YDB renderer gives it: the constraint's name, or `<table>_<columns>_key`.
A UNIQUE over the key folds into the key. The provider then creates and reads
an index, never a constraint. Schema validation and the creation projection
read the source the operator wrote past its UNIQUE, since the index changes no
column of the table; a UNIQUE the host does not turn into an index, such as a
deferrable one, is refused when it is planned.

The vector and full-text indexes of the table's native index set are built by
the creation itself, with the rules of
[Vector and full-text indexes](indexes-and-table-execution.md#vector-and-full-text-indexes), as the host's
embedded engine built them: the native `CreateTable` call carries each one as
the upstream arm of its index, and the TEXT form, an inspection and a
migration file write every index of the table inside its `CREATE TABLE`, in
name order, the vector or full-text one as `INDEX name GLOBAL USING
vector_kmeans_tree ON (...) WITH (...)`. YDB then builds no index over rows the
table holds, so the operation service lists no index build for the table. A
target that cannot build one refuses the whole creation before the table is
created.

A global index the native set states is refused at creation: a new
table takes its global indexes from the common declaration.

Access requirements, temporary tables, changefeeds, column
compression or encoding, and richer common constraints are refused.
Removal and rebuild are described below, and alteration covers column
families, the addition and removal of columns, the TTL, the automatic partitioning settings, the read replicas, the key bloom filter, and
the partitioning and read replicas of an index (see below). An unknown populated
field cannot disappear during compilation. A mixed SQL/native batch is validated
before its first unit executes.

Subprocess tests cover creation, inspection, stale plans, retained table data,
and refusal of a malformed later unit on YDB 25.1.4.7 and 26.2.1.14. Unit tests
exercise lost replies, lost confirmation, lease failure, and session cleanup.
The creation service is one part of the workflow. The
[host-driven plan test](../integration/host_migration_plan_test.go) also checks a
native table creation in Ptah's plan, applies it, verifies row retention through
later changes, and confirms convergence. Exact support still depends on the
requested table operation and source format.

### Serial columns

A column is a Serial when its type is `Serial`, `BigSerial` or `SmallSerial`
(or one of the spellings YDB shares with PostgreSQL: `SERIAL`, `SERIAL4`,
`BIGSERIAL`, `SERIAL8`, `SMALLSERIAL`, `SERIAL2`), or when an Int16, Int32 or
Int64 column is declared `auto_increment` or with an identity, as the host's
embedded engine reads it. The host sends the start and the increment of its
sequence with the common column, as `identity_start` and `identity_increment`:
Go and YAML write them as attributes of the field, and a YQL schema as the
`START WITH` and `INCREMENT BY` of an `ALTER SEQUENCE` on the sequence's
absolute path. No native facet holds them. A Serial is NOT NULL whatever the
declaration says, and its default is its sequence.

```go
//ptah:schema:field name="id" type="BIGSERIAL" primary="true" identity_start="100" identity_increment="5"
//ptah:schema:field name="id" type="BIGINT" primary="true" auto_increment="true" identity_increment="3"
```

The native CreateTable call creates the column with its sequence, named
`_serial_column_<column>`, starting at the declared start, stepping by the
declared increment and ending at the maximum of the column's type. Measured on
25.1.4.7 and 26.2.1.14, a sequence created this way issues its start first and
records no restart. The call gives a sequence the Int64 maximum unless the
request names one, whatever the column's width, so the request names the
maximum YQL's `Serial` and `SmallSerial` have. Execution confirms the sequence
by reading it back, and the operation payload is held to the same sequence, so
a forged request cannot name another one, count down or cycle.

The TEXT form writes the column as its Serial type and, for a start or an
increment other than 1, the `ALTER SEQUENCE` the host's embedded engine writes
after the CREATE TABLE, with `RESTART WITH` the start where it is not 1:
`START` alone leaves the next value at 1. The statement names the sequence by
its absolute path, so a creation that names no database, such as an offline
render's, is written without it, and the render reports the start and the
increment as omissions, as the host's embedded engine did (see [Offline schema
rendering](yql-reference.md#offline-schema-rendering)).

A plan against a database carries the
path of the database it read in the creation, since the host names the table it
plans by no database, so a migration file holds the statement, and YDB
records the restart, which it replays on every later `ALTER SEQUENCE`. A table
created from such a file therefore keeps the settings it was created with (see
below), where a table the native call creates can change them.

The planner refuses, in the embedded engine's words, an auto-increment column
of a type YDB has no Serial for, `GENERATED ALWAYS`, identity options other than
the start and the increment, a default on a Serial, a start or an increment
below 1 or not a whole number, and a start or an increment a creation or a
change would give a 16-bit or 32-bit Serial, a change back to the defaults
included. Every line reports `serial_sequence_keeps_range` false: any `ALTER
SEQUENCE` raises such a sequence's maximum to the Int64 maximum, and the column
then stores the value after its own maximum as a negative number without an
error. The native call could create such a sequence with its range kept, but no
later change and no migration file could, so it is refused at creation too.

A
table whose 16-bit or 32-bit Serial already holds the declared start and
increment needs no statement and plans nothing, and validation, which cannot
tell, takes the declaration, as the host's embedded engine does. A column table
takes no Serial, and YDB adds no Serial column to a table that exists (`Column
addition with serial data type is unsupported`).

When the declared start or increment of an existing Serial differs from what
the read reports, the planner writes one step, `serial-sequence/N`: one YQL
`ALTER SEQUENCE ... START WITH ... INCREMENT BY ...` on the sequence's absolute
path through the query service, outside a transaction and without a restart,
since a restart onto a value a row holds fails the next insert. The table
service has no call that changes a sequence: an AlterTable that names it
answers SCHEME_ERROR. Execution reads the column first and refuses one that
changed since the plan (`NOT_STARTED`), sends the statement once, and reads the
sequence back.

The next row still takes the value the sequence computed before
the change: with an increment of 3 moved to 7 after the rows 1 and 4, the next
rows are 7 and 14. The planner refuses to change a sequence some
`ALTER SEQUENCE` restarted, since the next insert would take the restart's
value again (`Conflict with existing key`), and the sequence of a 16-bit or
32-bit Serial. A column that becomes or stops being a Serial changes type,
which only a rebuild makes, and a rebuild of a table with a Serial is refused.

The session reports `serial_columns` and `serial_sequence_options` on every
line, and `serial_sequence_keeps_range` on none. Subprocess tests create a
Serial by the native call and by its TEXT form and change a sequence both ways;
host tests apply Go and YQL schemas with Serial keys, read the sequences back,
insert rows, change an increment, and show each refusal against the server
state that makes it necessary, on 25.1.4.7 and 26.2.1.14.

## Native table removal

A table the declaration no longer holds, which the host plans to drop, is
dropped by this provider: the planner replaces the host's steps for the table
with one removal step, and its receipts name it for every table facet. A row
table goes with its indexes and changefeeds, and a column table the same way.
The removal is written as YQL's `DROP TABLE`, as the host's embedded engine
writes it, in either form: execution runs it through the query service once,
outside a transaction, and confirms that the path is gone. A table whose read
records its changefeeds as uninspected or unrepresentable, and carries none, is
not dropped (`dropping table "app/t": the changefeed namespace is not fully
described: ...`): `DROP TABLE` takes every changefeed with it, and the plan
cannot tell whether a replication reads one.

Before the statement is sent, execution reads the path: it must still hold a
table of the store the plan read, with the columns the plan read, in order, and
the same key columns. A table that changed or went since the plan is refused
with nothing dropped (`NOT_STARTED`). This is not an atomic compare-and-set: the
native API has none. The live tests drop a row table natively and by text, a
column table, and refuse a table a column was added to after planning.

## Native table rebuild

YDB changes neither a table's key nor a column's type in place, makes no
column NOT NULL, and takes a starting layout only when it creates a table. With
the host's planning policy asking for a rebuild (contract
25.3, `planning.policy`; `--allow-table-rebuild` on the native CLI), the
planner makes such a change the way the host's embedded engine does, in five
steps and one more for each index whose held settings differ from a new
index's (see below): it creates the table anew from the declaration under a
scratch name (`__ptah_rebuild_<table>`), copies the rows with one `INSERT ...
SELECT`, renames the old table out of the way (`__ptah_replaced_<table>`),
renames the new one into its place, and drops the old one.

Where a table the
declaration holds or the plan drops already has a scratch name, the rebuild
takes the next free one (`__ptah_rebuild_<table>_1` and on). The host's steps for the table
are rewritten to the rename that puts the new table in place. A REBUILD the
host proposes is planned the same way, and refused like any other rebuild
without the policy.

A key change reaches the planner from a host that lists
`schema.observed-primary-keys` and `planning.primary-key-changes` (contract
25.26): the read reports each table's key in key order, under the name
`<table>_pkey` the host's embedded engine gave it, so a declaration that orders
the same columns otherwise is a key change. The host hands it over as an
alteration of the table, and the planner rebuilds the table with the declared
key, or without the policy refuses it as the embedded engine did:
`adding constraint <table>_pkey, the primary key, which requires target
capability primary_key_alterable, unavailable on this ydb target`. The reverse
plan rebuilds the table with the key it held.

The copy reads each column as the new table declares it. A changed type is
converted with `CAST` and unwrapped, so a value that does not convert fails the
copy with a message naming the table and the column instead of becoming NULL;
a column made NOT NULL is unwrapped too, which fails the copy on a NULL row.
The copy is one data query, so YDB's refusal shows that nothing of it applied:
`schema apply` stops PARTIAL after the new table's creation, and the reason
carries the message (see [Error reasons](catalog-and-errors.md#error-reasons)).

The new table keeps the old table's attributes, and with them the comments Ptah
keeps there, and each storage setting the declaration does not state, as the
old table holds it: every column family, the default one and one no column
sits in included, with the settings the declaration leaves out (its cache mode
among them, which a target without `column_family_cache_mode` refuses to
write), the family each column sits in where the declaration does not say (see
[Column families](table-settings.md#column-families)), the automatic partitioning, the read replicas, the key bloom
filter (`KEY_BLOOM_FILTER = DISABLED` for a table that states none, as the host's
embedded engine names it), the TTL where the declaration states no table properties, and the run
interval of the TTL it holds where the declaration states a TTL, which names
none since YQL has no setting for one.

A cluster's table profile may have
given the old table any of them, and YQL drops neither a family nor a setting,
so a rebuild changes none that nobody declared. Each index of the new table
takes the automatic partitioning and the read replicas its own facet declares
(see [Index settings from a source](indexes-and-table-execution.md#index-settings-from-a-source)), and for
every setting the facet leaves out what the old index under its name holds,
or the old index the host renames it from, as the host's embedded engine
resolves a rebuilt index's declaration. An index the rebuild adds takes its
declared settings over the ones YDB gives a new index.

The call that creates a
table takes no index settings, so a `table-rebuild/N/index-settings/M` step
after the creation and before the copy gives the index every setting with
`ALTER TABLE ... ALTER INDEX ... SET`, over the settings YDB gives a new index.
The read describes each index the common model describes beside the native
index set for that reason. A facet for an index the new table does not create
refuses the rebuild. Without the policy, the change is refused in the embedded
engine's words, naming the request the host passes, such as
`--allow-table-rebuild`.

The host's own steps for the table, such as the addition or the removal of an
index or a column, are rewritten to the rename that puts the new table in
place, and that rename names every object they change, as the host's plan
graph requires. An index the plan drops and adds again under its name, such as
one whose columns change, cannot be named twice by one step, so the removal,
and every removal the host orders before it, is rewritten to the rename that
moves the old table out of the way. A host step that changes something before
such a removal refuses the rebuild.

Every step writes as a YQL statement in the TEXT form, so a migration file
holds the rebuild, unless the old table holds user attributes or a TTL run
interval other than YDB's own: YQL's CREATE TABLE sets neither, so such a
rebuild has no TEXT form (`NO_TEXT_FORM`) and runs natively. A native execution checks each step's precondition first:
the copy needs the new table empty and the source holding every copied column,
a rename needs the table holding the captured columns and key, and a free
target.

YDB renames no table that carries a changefeed, so a rebuild of a
table that holds changefeeds drops them in a step before the old table moves
and gives the new table its changefeeds in a step after it is in place: the
ones the declaration states, or the ones the old table held where the
declaration does not state them, with their consumers. Each stream restarts.
The rebuild's first step carries the note, after the rebuild's own notes, in
the host's embedded engine's words: `Changefeed feed of table app/cf is dropped
before the swap and added again after it, because YDB moves no table that
carries a changefeed`, and a changefeed the new table does not get is noted as
dropped with its topic.

A changefeed an asynchronous replication
controls refuses the rebuild, as does a column table, whose rebuild needs an
explicit data migration. So does a read that does not say which changefeeds
the table holds, since the rename would fail after the new table is created and
filled: a read that captured none while the declaration states some, a read
whose coverage records them as uninspected or unrepresentable and carries none,
as the host records them for two HCL documents (`the changefeed namespace is
not fully described: ...`), and a changefeed the read cannot represent.

A native execution gives the new table the access control entries the old
table holds before it copies the rows, as the host's embedded engine gave a
rebuilt table back its grants: the new table is created with its directory's
permissions alone, and the old one is dropped with its own. A grant or revoke
the plan makes on the table runs after the rebuild, on the new table.

A
migration file runs later and reads no server, so under `planning.table-grants`
(contract 25.33, ADR 0111 in Ptah), where the host sends the grants the read
found on each held table, the TEXT form grants them on the new table after the
old one is dropped, where the host's embedded engine wrote them, one `GRANT` to
a line: every grant except a privilege the table holds by default and one the
plan revokes. They follow the drop so the rebuild's statements stay the
consecutive steps the migration lint reads as one rebuild. A read that did not
describe the table's grants leaves the file without them, and a note above the
rebuild says so; a host that does not send the grants leaves the new table
with its directory's permissions.

The live tests rebuild a table natively and by text on both release lines,
read the copied rows back, and fail a copy whose value does not convert.

A table whose declaration holds a Serial column is not rebuilt: the new
table's sequence would start at 1 while the copied rows keep their values, so
the next insert would collide (`Conflict with existing key`), and `ALTER
SEQUENCE ... RESTART WITH` takes only a literal, so no statement can move it
past the copied rows.
