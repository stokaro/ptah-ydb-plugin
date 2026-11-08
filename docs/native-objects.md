---
title: YDB native object lifecycles
description: Coordination nodes, changefeeds, external objects, and secrets.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How does the provider plan changes to YDB native objects?"
goal: "Find YDB native-object lifecycle rules and refusals."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 12
---

# YDB native object lifecycles

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Native coordination planning and execution

The offline planner preserves complete before and after operands. Each step
has an object effect, a shared `ydb.scheme-path` effect, and a forbidden SQL
transaction requirement. The path effect names no database, as the host names
a path of the session's database. Table-to-node and node-to-table replacements
require an explicit common path handoff. A view the host creates or drops
occupies its path as a table does: a node beside it needs no order, and a node
that takes the path of a dropped view comes after the drop. A grant or a revoke
on a table or a view, and a comment, change a resource of the object's own and
occupy no path; the step's read of the object carries the path.

YDB needs every
path above an object to be a directory, so a node created below a path follows
the step that drops the object there, and a node dropped at a path precedes a
step that creates an object below it. That holds whichever planning service
plans the other step: a drop records a `ydb.scheme-directory` creation at its
path, a creation reads each directory above its own, and the host runs a
creation before another service's read. A drop below a path where the plan
creates an object is refused before anything runs, since YDB keeps a directory
after its last object goes and YQL has no statement that removes one.

The
refusal is the host's embedded engine's, `this plan drops app/holder/events and
creates an object at app/holder, which is a directory in the database`, also
where two planning services plan the two objects, and where both are tables. A
service sees only its own changes, so the pair of two services is refused in
the streaming query service's call, which the host asks on every plan with the
desired and current objects of every kind and the subject of every change. Any
other overlap of parent and descendant paths, and an unsupported common
footprint, is refused. The host's window markers (such as `common/feature-creations`) and the
column of a column step occupy no path, so they order nothing; the host places
the step between its markers.

A common step that names another database than
the change is refused. A parent directory that does not exist holds no node,
and CreateNode creates it with the node; execution never deletes a directory.

Rendering validates the complete operation receipt and emits a native envelope,
or, in the TEXT form, the coordination node statement the host's embedded
engine writes (see [Text units](connections-and-execution.md#text-units)). Execution uses native CreateNode,
AlterNode, and DropNode requests. A nonempty database identity must equal the
selected session database. The provider validates every envelope and transaction
constraint before executing any unit in the batch.

Before each native write, execution lists the parent directory, or proves
that it does not exist, and reads the node. The current configuration and
access metadata must match the captured observation, or creation must have
established absence. A stale operation is refused before dispatch. Effective settings are sent explicitly, including
omitted settings that reset to defaults. Alteration preserves the existing node.
After a successful reply, execution reads back the configuration or confirms
absence; alteration also checks that access metadata did not change.

A refusal before dispatch reports NOT_STARTED, or PARTIAL after an applied
prefix. A failed or unconfirmed dispatched request reports UNKNOWN and is never
retried. Native operations cannot join SQL transactions. Lease checks run before
dispatch and guard the call, as they do for SQL execution.

These checks are not an atomic compare-and-set. Another actor can change a node
between validation and dispatch; the native API has no conditional schema-write
contract. Successful creation does not establish exclusive ownership. The SDK
subprocess tests cover create, alter, reset, drop, stale plans, mixed SQL/native
batch refusal, and lease loss on both tested YDB release lines. The host routes
a coordination node's comparison, planning and execution to these services
(see [Coordination comparison](coordination-models.md#coordination-comparison)).
Other native-object lifecycles are described in the following sections.

## Changefeeds

The provider declares, compares, plans and applies the changefeeds of row
tables and the consumers of their topics, with the syntax, the rules and the
statements of the host's embedded YDB engine (`internal/ydbchangefeed`).

A YAML table declares its changefeeds under the table key `changefeeds`, and a
Go file with the directives `ptah:schema:changefeed` and
`ptah:schema:changefeed:consumer` on the table's struct, or on another struct
with the `table` attribute:

```yaml
tables:
  orders:
    columns:
      id: {type: Uint64, primary: true}
    changefeeds:
      updates:
        mode: NEW_AND_OLD_IMAGES
        format: JSON
        retention_period: PT12H
        consumers:
          billing:
            important: true
          search: {}
```

```go
//ptah:schema:table name="orders"
//ptah:schema:changefeed name="updates" mode="NEW_AND_OLD_IMAGES" format="JSON" retention_period="PT12H"
//ptah:schema:changefeed:consumer name="billing" changefeed="updates" important="true"
//ptah:schema:changefeed:consumer name="search" changefeed="updates"
type Order struct {
	//ptah:schema:field name="id" type="Uint64" primary="true"
	ID uint64
}
```

The options are the host's: `mode` and `format`, which YDB requires,
`virtual_timestamps`, `resolved_timestamps`, `initial_scan`, `user_sids`,
`schema_changes`, `topic_min_active_partitions`, `topic_auto_partitioning` and
`retention_period`; a consumer takes `important`, `read_from`,
`supported_codecs` and `availability_period`. Each is parsed and refused with
the host's words, and a Go consumer names a changefeed its table declares. A
YAML or Go source describes the changefeeds of every table it declares, so a
table it declares with none is declared to have none and the changefeeds it
holds are dropped. A YQL source cannot declare one (see
[SQL schema reading](yql-reference.md#sql-schema-reading)) and leaves them as they are. A
column table takes no changefeed.

The comparison reads both sides as the host's specification: a mode, a format
and a codec in any case, an interval by the seconds it denotes, YDB's 24 hours
for a retention left out, consumers by name and their codecs as a set, and a
topic's starting partition count only where both sides name one. The
statements of a change list the consumers as Ptah's built-in YDB engine did:
those it drops in the order the read lists them, which is name order, and those
it adds in the order the declaration lists them, so the rollback of a change
drops the consumers it added in the order it added them.

A changefeed
the read cannot represent (an AWS region, attributes, a fraction of a second,
an auto-partitioning strategy no changefeed declares, a consumer attribute
other than YDB's own) leaves the table's changefeeds undecided, and so does a
change to a stream an asynchronous replication controller owns, or a
declaration of such a stream that does not retain the controller's binding, as
a desired side converted from a read retains it; such a stream the declaration
leaves out is kept. A binding is read only with every field, an absolute
replica path, a canonical positive item number and no other key; any other is
an attribute the read cannot represent.

YDB drops a table whose stream a
controller owns and leaves the replication failing (`Discovery for all topics
failed`), so such a table is dropped only with a replication of this database
the plan drops too. The table planner refuses the drop where the binding names
a replica outside the database, in a database nested in it, or at a path where
this database holds no table, which is a database of the same name on another
server; the read lists the replica's path to tell. The replication planner
refuses it where the plan keeps the replication.

The table lifecycle service plans every changefeed change of a table as one
step, outside a transaction: the host runs a step that drops an object after
every step that creates one, and YDB keeps a limit on the changefeeds of one
table, so a swap at the limit and a changefeed dropped and added again need
one step. The operation runs the host's statements: `ALTER TABLE ... DROP
CHANGEFEED` for every removal, then `ALTER TABLE ... ADD CHANGEFEED` with an
`ALTER TOPIC ... ADD CONSUMER` per consumer for every addition, then `ALTER
TOPIC` for a retention or a consumer that changes in place.

YDB changes no
option of a changefeed in place (`MODE alter is not supported`), so such a
change drops the changefeed and adds it again, and a consumer whose codec list
the declaration empties is dropped and added again. A disabled changefeed the
declaration states disabled, as one converted from a read does, keeps its state:
its topic changes in place, since `ALTER TOPIC` disables nothing, and no step
adds a disabled changefeed, since no statement disables one. A new table's changefeeds
follow its creation.

The planner refuses an option the session lacks with the
host's capability key: `changefeed_user_sids` before 26.1,
`changefeed_schema_changes` before 25.3, `changefeed_topic_auto_partitioning`
before 25.2 or with the feature flag off, and a consumer's
`availability_period` before 25.4. It refuses the shapes YDB refuses on every
line, a topic of several partitions on a first key column other than Uint32
or Uint64, a changefeed named after an index of its table, and a changefeed
and an index that trade a name in one plan. A removal or a recreation, and a
consumer dropped or restarted, is destructive.

Execution reads the table and each changefeed's topic before the first
statement and refuses a changefeed that changed since the plan read it; it runs
each statement once and reads the table back, requiring each declared
changefeed with its topic's retention and consumers, or a removed one's
absence. The one statement that runs again is one YDB refuses because another
schema operation holds the table's path (OVERLOADED, `path ... has been locked
by tx` or `path is under operation`), which changed nothing: a changefeed added
with an initial scan holds the path until the scan has read every row, so the
next statement waits, 50 ms at first and up to a second between attempts,
until it runs or the request's context ends. Any other unconfirmed result is
UNKNOWN and is never retried.

In the TEXT form a changefeed operation is the statements the host's embedded
engine writes into a migration file for the same change, one to a line, in its
order: the removals, a changed changefeed among them, then the additions, each
with the consumers of its topic, then each topic's change in place. A
migration runner runs each statement alone, so an `ALTER TOPIC` finds the
topic the `ADD CHANGEFEED` above it created. A migration file then carries a
changefeed change, and a rebuild of a table that holds changefeeds.

`TestChangefeedLifecycleSubprocess` adds a changefeed with an initial scan and
two consumers, restarts a consumer, refuses a change planned before another
writer changed the topic, drops and adds the changefeed for a mode change, and
drops it, reading the server back after each step.
`TestHostAppliesChangefeeds_HappyPath` drives `ptah schema apply` from a YAML
and a Go source: it creates a table with two changefeeds, finds nothing to
change, changes a topic in place and recreates a changefeed, finds nothing to
change again, and drops both.

## External data sources and external tables

The provider declares, compares, plans and applies YDB external data sources
and the external tables over them, with the syntax, the rules and the
statements of the host's embedded YDB engine (`dialect/ydb/ydbexternal`). One
comparison service and one planner own both kinds, because a data source the
plan drops and creates again takes the tables over it along.

A YAML document declares them under the document keys `external_data_sources`
and `external_tables`, a Go file with the directives
`ptah:schema:externaldatasource` and `ptah:schema:externaltable`, and a YQL
schema with `CREATE [OR REPLACE] EXTERNAL DATA SOURCE` and `CREATE [OR REPLACE]
EXTERNAL TABLE`; OR REPLACE is not part of the declaration, because whether a
plan replaces an object is the plan's decision:

```yaml
external_data_sources:
  events_bucket:
    schema: ext
    source_type: ObjectStorage
    location: https://storage.example.test/events/
    auth_method: NONE
external_tables:
  events:
    schema: ext
    data_source: ext/events_bucket
    location: 2026/
    columns:
      - {name: id, type: Int64, not_null: true}
      - {name: kind, type: Utf8}
    options:
      FORMAT: csv_with_names
      CSV_DELIMITER: ";"
```

```go
//ptah:schema:externaldatasource name="events_bucket" schema="ext" source_type="ObjectStorage" location="https://storage.example.test/events/" auth_method="NONE"
//ptah:schema:externaltable name="events" schema="ext" data_source="ext/events_bucket" location="2026/" columns="id Int64 NOT NULL, kind Utf8" options="FORMAT=csv_with_names;CSV_DELIMITER=\;"
type Externals struct{}
```

A data source takes `name`, `schema`, `source_type`, `location`, `auth_method`
and `options`; a table takes `name`, `schema`, `data_source`, `location`,
`columns` and `options`. A Go `options` attribute is `NAME=value` pairs
separated by `;`, where `\;` writes a semicolon and `\\` a backslash, and a Go
`columns` attribute is `name Type [NOT NULL]` entries separated by commas.
Option names are read in upper case; an option that has an attribute of its
own, `REFERENCES`, which the server keeps, a column with `DEFAULT`, `PRIMARY
KEY` or `FAMILY`, and an object declared twice are refused with the host's
words.

Each source describes the external objects of the directories it reads
completely, so an object it leaves out is dropped; a `ptah:not-described
external_data_source` or `external_table` header entry of a YQL document keeps
one. HCL has no block for either kind.

The comparison reads both sides as the host does, against the database the
current side was read from: source type, location and auth method exactly,
options by upper-case name, and a `*_SECRET_PATH` option relative to the
database, which the server keeps absolute; a table's data source relative to
the database, its location, its columns in order with their types and
nullability, and its options, with `PARTITIONED_BY` read as the list it holds.
REFERENCES and the access metadata are not compared. A declaration on a
target without `external_data_sources` is refused, and so is a plan that drops
a data source a declared table reads. A data source dropped and created again,
or replaced with a new source type, takes each declared table over it along,
as a change whose operands may describe the same table.

YDB alters neither object (`Alter operation for EXTERNAL_DATA_SOURCE objects is
not implemented`), so the planner replaces a changed one: with `CREATE OR
REPLACE` where the session reports `external_object_replace`, and otherwise by
a DROP and a CREATE. A data source dropped and created again is one operation;
a table is a drop before the statement on its data source and a restore after
it. Tables are dropped before the data sources they read and created after,
and every step asks for the host's dependent phase, the one window creations
and drops share.

YDB refuses to drop or replace a data source a column table's
TTL names, so the TTL of a column table the plan keeps is reset before a
replacement of such a source and set again after it, as the host's embedded
engine carries it (a target without `tiered_ttl` refuses the replacement), a
column table the plan drops is dropped before the sources its TTL names, and a
plan that leaves a table with a TTL naming a data source it drops is refused
(`its TTL reads external data source ..., which the plan drops`).

The planner refuses, with the host's words, every statement
on a target without `external_data_sources`, an option ending in
`_SECRET_PATH` before 25.4, where the session reports
`external_data_source_secret_paths` false, a secret path outside the
database, two objects at one path, and a table over a data source the batch
writes that is not ObjectStorage. No statement holds data YDB stores, so a
drop or a replacement is behavioral and never destructive.

Execution reads the object before the first statement and refuses one that
changed since the plan read it, REFERENCES aside, or a path that holds
something else. It runs each statement once, gives an object dropped and
created again the owner and the direct access entries the old one held, and
reads the object back, requiring the declaration, the owner and the access
entries, or the object's absence after a drop. An unconfirmed result is
UNKNOWN and is never retried. A dev realm session refuses both kinds, which
reach past the realm.

`TestExternalLifecycleSubprocess` creates a data source and a table over it,
moves the data source with access entries on both objects, refuses a change
planned before another writer changed the table, and drops both, reading the
server back after each step. `TestExternalReplacementSubprocess` turns
EnableReplaceIfExistsForExternalEntities on through the server's dynamic
configuration and replaces the data source, which keeps the table, and then the
table in place, each keeping its owner and access entries.
`TestHostAppliesExternalObjects_HappyPath` drives
`ptah schema apply` from a YAML, a Go and a YQL source: it creates both
objects, finds nothing to change, moves the data source, adds a column to the
table, finds nothing to change after each, and drops both.

## Secrets

The `ydb-secrets` comparison and planning services manage scheme-secret
presence. Inspection records the secret's path without reading its value.
A declaration names an approved environment input:

```yaml
secrets:
  pg_password:
    schema: app
    value_env: PTAH_SECRET_PG_PASSWORD
```

The equivalent Go annotation is
`//ptah:schema:secret name="pg_password" schema="app" value_env="PTAH_SECRET_PG_PASSWORD"`.
A YQL schema declares
`CREATE SECRET \`app/pg_password\` WITH (value = $PTAH_SECRET_PG_PASSWORD);`.
Schema sources refuse inline values. Input names must start with `PTAH_SECRET_`
and contain only ASCII letters, digits, and underscores. A refused declaration
gives the reason a host prints, in the words of Ptah's built-in YDB engine: the
secret, by its YAML key, the attribute and what it takes. It never repeats the
value a declaration wrote or the variable it names:

```text
provider decode source failed (InvalidArgument): secret "pw": invalid value_env: the variable it names does not start with PTAH_SECRET_ and a name after it; Ptah reads a secret's value only from a variable under that prefix, so a migration file cannot copy any other variable of the machine that applies it
```

An absent secret is created. An existing secret is unchanged unless the
operator requests rotation with `--rotate-secret app/pg_password`. Rotation
of an absent declared secret creates it. A rotation request must name a
secret retained by the selected desired schema. An omitted secret is removed
only when desired and current coverage establish that removal. A declaration
that omits a secret a declared object still reads by its path is refused: YDB
drops a secret an external data source, an async replication or a transfer
names and keeps the object, which then names a secret that is gone.

The `units.sensitive-inputs` service reports input names before approval.
Previews and saved operations contain references only. Immediately before
execution, the host checks those references again and resolves the approved
inputs. The provider receives ephemeral bindings and never reads its process
environment for secret values. A missing binding refuses the entire batch
before its first write; an explicitly empty value is accepted. SQL migrations
can use these references as the complete `value` of `CREATE SECRET` or
`ALTER SECRET`. Expressions, assignments, and references in unrelated
statements are refused. Query errors redact expanded values.

Native execution checks presence before dispatch and again afterward. Rotation
also confirms that the owner and permissions remain unchanged. Values cannot
be read back, so these checks cannot detect a concurrent value rotation.
Secret operations run outside SQL transactions and are never retried
automatically. Targets without the `secrets` capability refuse declarations
and mutations. Any secret names still returned by their directory listing
remain unknown observations; they cannot become planned removals.

External data source planning records its old and new secret-path
dependencies. Creation follows its secret's creation, and a reference is
removed before its old secret is dropped. Absolute references must resolve
inside the selected database or dev realm. The creation or change of an async
replication or a transfer reads the secret its connection names by path,
since YDB looks the secret up when it creates the object or changes its
connection, so it follows the secret's creation; a path outside the database
is refused.
