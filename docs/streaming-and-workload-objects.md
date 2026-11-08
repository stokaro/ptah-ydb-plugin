---
title: YDB streams, replication, and workloads
description: Topics, streaming queries, async replication, transfers, resource pools, and classifiers.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How are YDB streaming and workload objects managed?"
goal: "Find planning rules for YDB streams, replication, transfers, and workloads."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 11
---

# YDB streams, replication, and workloads

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Topics

The provider declares, compares, plans and applies standalone topics and their
consumers, with the syntax, the rules and the statements of the host's embedded
YDB engine (`dialect/ydb/ydbtopic`).

A YAML document declares topics under the document key `topics`, a Go file with
the directives `ptah:schema:topic` and `ptah:schema:topic:consumer`, and a YQL
schema with `CREATE TOPIC` and `ALTER TOPIC ... ADD CONSUMER`:

```yaml
topics:
  order_events:
    schema: app
    min_active_partitions: 2
    retention_period: PT36H
    supported_codecs: [raw, gzip]
    consumers:
      billing:
        important: true
      audit:
        read_from: "2026-01-01T00:00:00Z"
```

```go
//ptah:schema:topic name="order_events" schema="app" min_active_partitions="2" retention_period="PT36H" supported_codecs="raw,gzip"
//ptah:schema:topic:consumer name="billing" topic="order_events" schema="app" important="true"
//ptah:schema:topic:consumer name="audit" topic="order_events" schema="app" read_from="2026-01-01T00:00:00Z"
type Events struct{}
```

The settings are the host's: `min_active_partitions`, `max_active_partitions`,
`auto_partitioning_strategy`, the up and down utilization percentages and the
stabilization window, `retention_period`, `partition_write_speed_bytes_per_second`,
`partition_write_burst_bytes` and `supported_codecs`; a consumer takes
`important`, `read_from`, `supported_codecs` and `availability_period`. Each is
parsed and refused with the host's words. A Go consumer names a topic the same
file declares. Each source describes the topics of the directories it reads
completely, so a topic it leaves out is dropped; a `ptah:not-described topic`
header entry of a YQL document keeps one. HCL has no topic block.

The model
keeps the consumers of a declared topic in the order the declaration lists
them, and those of a topic it reads in the order YDB describes them, which is
the order they were added in, so a statement Ptah writes for a topic lists
them as Ptah's built-in YDB engine did.

A setting a declaration leaves out takes the value YDB gives a new topic, and
the comparison reads both sides that way: one partition, a day of retention, a
write speed of 1 MiB/s and a burst of the speed, and with auto-partitioning on,
the maximum at the minimum, 90% and 30% thresholds and a five-minute window.
Consumers compare by name. YDB's own attributes and the access metadata are not
compared. A topic the read finds holding a setting no statement sets
(`retention_storage_mb`, `partition_count_limit`, a metering mode, the read
speeds or a metrics level), or a consumer whose codec list names a codec no
declaration names, refuses the comparison as the host refuses to read it,
whatever the source says of the topic: `YDB topic <path>: <why>`, the path
from the database root.

The planner writes one step per topic, outside a transaction. It refuses a
change YDB cannot make in place: fewer partitions than the topic has, and
switching auto-partitioning off once it is on (declare `paused` instead). It
refuses a consumer's `availability_period` on a server before 25.4, where the
session reports `topic_consumer_availability_period` false. The operation runs
the host's statements: one `CREATE TOPIC` naming what the declaration names, one
`DROP TOPIC`, or one `ALTER TOPIC` that names every setting at its resolved
value once any differs, drops, alters and adds consumers, and a second `ALTER
TOPIC` that adds again a consumer whose codec list the declaration empties,
which YDB changes only by dropping the consumer. A drop of a topic or of a
consumer is destructive.

Execution reads the topic before the first statement and refuses one that
changed since the plan read it, or a path that holds something else; it runs
each statement once and reads the topic back, requiring the declared settings
and consumers, the access metadata an alteration found, or the topic's absence
after a drop. An unconfirmed result is UNKNOWN and is never retried.

`TestTopicLifecycleSubprocess` creates a topic, restarts a consumer, refuses a
change planned before another writer changed the topic, and drops it, reading
the topic back from the server after each step. `TestHostAppliesATopic_HappyPath`
drives `ptah schema apply` from a YAML, a Go and a YQL source: it creates a
topic with its consumers, finds nothing to change, changes the retention and
the consumers, finds nothing to change again, and drops the topic.

## Streaming queries

The provider reads, compares, plans and applies streaming queries from YAML,
Go and SQL sources. A session enables `streaming_queries` only when the target
reports both `EnableStreamingQueries` and `EnableExternalDataSources` through
its monitoring endpoint. A target without that evidence refuses a streaming
operation. A listed query whose system view cannot establish every setting is
uninspected, so an incomplete reading cannot authorize its removal.

```yaml
streaming_queries:
  copy_events:
    schema: app
    text: INSERT INTO `app/sink` SELECT * FROM `app/source`;
    run: true
    resource_pool: default
    allow_state_reset: false
```

The Go directive is `ptah:schema:streamingquery`, with the same attributes and
an explicit `name`. SQL accepts `CREATE STREAMING QUERY ... DO BEGIN ... END
DO`, including `IF NOT EXISTS` and `OR REPLACE`. `OR REPLACE` grants the same
state-reset permission as `allow_state_reset: true`. HCL has no streaming
query block. A complete source drops a query it omits; the SQL header
`ptah:not-described streaming_query` preserves queries outside its coverage.

An omitted `run` means true and an omitted resource pool means `default`.
Comparison ignores query whitespace and comments but preserves literal values.
A changed body requires explicit state-reset permission because it discards
checkpoints. Stopping or changing the resource pool keeps topic offsets. The
planner stops a running query before changing its inputs and restarts it after
its new inputs exist; a change that only stops the query emits one statement.

A creation, a change or a restart runs after the host's common steps, and a
grant on the query after it, as the YDB engine built into the host at
74de0b65b placed them: measured on 26.2.1.14, a query between two topics beside
a group, a table and grants on the table, a third topic and the query plans
the topics, the group, the table, the grants on the table and the topic, the
query and then the grant on it. A principal change after a grant on the query
waits for the grant, so the query runs ahead of that change. A stop and a drop
run early.
This assessment also runs when the query declaration is unchanged. Captured
query values require `planning.object-context` (contract 25.7), and each query
receives an assessment receipt.

A changed input restarts a running query only
when the desired capture establishes every surviving input and resource pool.
A resource pool altered in place is no changed input: YDB runs `ALTER RESOURCE
POOL` beside a query running in the pool and the query keeps running, measured
on 26.2.1.14, so the plan alters the pool alone, as the host's embedded engine
did. A plan that drops the pool a running query names is refused.
Missing definitions, conflicting occupants, and foreign database inputs refuse
the complete plan. An unscoped inspection walks every directory for streaming
queries, including dot-prefixed user directories. A failed directory listing
refuses the read; an unread query keeps explicit unknown coverage.

Scoped
inspection and reads that exclude other dev realms cannot establish the full
dependent set, so input mutations refuse when that inventory is incomplete.
A disabled query reader does not prove absence: directory listings do, and a
listed unread query remains unknown. Stopped queries retain their stopped
state. Native and text rendering carry the same operations and preconditions.

Input dependencies use physical scheme paths, including absolute paths inside
the selected database or dev realm. A constant `PRAGMA TablePathPrefix` applies
to subsequent relative paths. A foreign path or an unresolved prefix refuses
planning. Constant named paths, named subqueries, `AS_TABLE`, static `CONCAT`
and `EACH` inputs retain their dependencies. A qualified external reference
retains the local external data source as its dependency. Unresolved named
paths, `USE`, and dynamic table functions such as `RANGE` refuse planning:
their complete physical inputs cannot be proven from the captured body.
Ordinary strings outside a table expression create no path dependency.

A named resource pool remains a database-wide dependency, including
a literal dot in its name, and a query that names none depends on the pool
`default`. Scope selection keeps that pool when it keeps the query, unless
the pool is explicitly excluded, so an `--include` of a query plans the change
of the pool it runs in, as the host's embedded engine did. Access changes follow a query's
stop and restart; a final drop accounts for revoking that query's access.

Execution verifies the stored query settings before dispatch, checks
cancellation and held locks before each statement, and reads back the result.
This precondition contains no ACL or principal state; access ordering remains
explicit in the complete plan. Each statement
runs once. A dispatched operation whose result cannot be confirmed is UNKNOWN.
The dev-realm guard checks the query body as well as the query's own path.

`TestHostStreamingQueryGraph` exercises the combined host graph on YDB 26.2:
it stops a query, removes its old source, replaces a table with the new source
topic at the same path, restarts the query, verifies a synced second apply,
and stops it without changing its body.

## Async replications and transfers

The provider reads, declares, compares, plans and applies YDB async
replications and transfers, with the rules and the statements of the host's
embedded YDB engine (`dialect/ydb/ydbreplication`). The domain rules live in
`internal/replication`.

A YAML document declares them under the document keys `async_replications` and
`transfers`, and a Go file with the directives `ptah:schema:async_replication`,
`ptah:schema:async_replication:item` and `ptah:schema:transfer`, with the
host's spelling. In YAML a `name` key overrides the entry's key; slashes around
`schema` are dropped:

```yaml
async_replications:
  mirror:
    schema: replicas
    connection_string: grpcs://primary:2135/?database=/prod
    user: replicator
    password_secret_name: password
    consistency_level: global
    commit_interval: PT30S
    items:
      - source: accounts
        target: replica/accounts
transfers:
  ingest:
    source: orders/feed
    target: order_log
    using: "($m) -> { return [<| id: $m._offset |>]; }"
    flush_interval: PT10S
```

```go
//ptah:schema:async_replication name="mirror" schema="replicas" connection_string="grpcs://primary:2135/?database=/prod" user="replicator" password_secret_name="password"
//ptah:schema:async_replication:item replication="mirror" schema="replicas" source="accounts" target="replica/accounts"
//ptah:schema:transfer name="ingest" source="orders/feed" target="order_log" using="($m) -> { return [<| id: $m._offset |>]; }"
type Replication struct{}
```

A replication takes `connection_string`, one credential (`token_secret_name`,
`token_secret_path`, or `user` with `password_secret_name` or
`password_secret_path`), `consistency_level`, `commit_interval` and at least one
item. A transfer takes `source`, `target`, `using`, the connection settings,
`consumer`, `batch_size_bytes` and `flush_interval`. Each is parsed and refused
with the host's words. A Go item joins the replication of its name and
directory in the same file. A YQL schema declares both through [SQL schema
reading](yql-reference.md#sql-schema-reading). An HCL file declares neither yet.

Inspection describes each `REPLICATION` and `TRANSFER` entry through the
replication service: the connection string rebuilt from its parts, the
credential by the secret it names, the items with their sources relative to the
source database and their targets relative to the database root, the
consistency and batch settings where they differ from YDB's defaults, a
transfer's lambda as written, and the state (running, paused, done or error).
local-ydb leaves the replication API out of its gRPC services unless
`YDB_GRPC_SERVICES=replication` names it; on such a cluster, and for a
description the provider cannot model, the object is reported uninspected with
the reason, and the rest of the snapshot is read.

A table an async replication
writes (attribute `__async_replica`) is left out of the tables: YDB refuses
every write and ALTER on it, and a plan that read it as an ordinary table would
drop or alter it.

A replica no replication writes any more, left behind by a replication dropped
without `CASCADE`, stays read-only for good, and YDB takes no table at its
path. Where the host lists `schema.unread-tables`, the read names each such
table in `unread_tables` with the reason the host's embedded engine gives, so
the host refuses a schema that declares a table there before any step and
prints a note on `db read` and `schema inspect`. A replica that a replication
of the read writes is not named; the planner refuses a table declared there. A
filtered read lists the database's replications to tell the two apart when its
directories hold a replica, and a replication left uninspected makes the
reason say only that the read cannot describe the replication that writes it.

Without the feature, such a table is left out silently, and a declared table
there is refused when its creation runs (`NOT_STARTED`). Either way the replica
stays until an operator drops it by hand with `DROP TABLE`, which YDB accepts;
a schema that leaves it out plans nothing for it.

The comparison resolves both sides as YDB keeps them: the connection string in
its canonical form, the consistency level `row` and a global commit interval of
ten seconds where none is named, the batch size 8 MiB and the flush interval a
minute, a transfer's consumer as any where the declaration names none, and a
replication's items by the tables they replicate. An object the desired source
does not describe is kept; a replication YDB reports with no item, one that
failed before it resolved its tables, has no declaration that keeps it and is
left undecided. Session capabilities: `async_replication` on every line
(`EnableReplication` on 26.2), `transfers` from 25.2 (`EnableTopicTransfer`),
and `replication_secret_paths` from 25.4.

The planner writes one step per object, outside a transaction, and refuses what
the host refuses: a change of a replication's items, consistency level or
commit interval, or of a transfer's source, target or consumer, which YDB makes
in no object in place; a connection or credential change of an object that is
not paused, or of a replication that was failed over; a credential taken away;
a secret path before 25.4.

It refuses a drop of a running replication whose
replica the plan declares as a table, a creation where a table stands at a
target, and a creation whose target lies at, above or below the target of
another replication the plan keeps or creates: YDB creates the replication over
the other's replica and both write it, and a later drop of either with
`CASCADE` drops the replica while the other runs on without it, measured on
25.1.4.7 and 26.2.1.14. Where the host negotiates `planning.object-context`, the planner also
assesses every replication the plan keeps, and refuses a table the plan
creates at the path of a live replica, and a failed-over replica the plan drops
while the schema keeps the replication.

A creation names its replica paths as created and a drop of a running
replication with `CASCADE` names them as dropped, so each step is ordered
against the common steps at those paths, and a creation comes before a view
the plan creates over one of its replicas. A replication of this database also
reads its source tables, so it is created after a table the plan creates and
dropped before one the plan drops: a replication created before its source
creates no replica. A transfer reads its table and, at a local source, the
topic or the changefeed of a table.

It names the changefeed as the changefeed
planner does, so the host creates it after a changefeed the plan adds; a
dropped transfer names the topic by the topic's identity rather than its path,
so the drop comes before the topic's even where the plan creates another object
at that path. A creation or a change of either object reads the secret its
connection names by path, so the plan creates the secret first,
and a source that drops the secret while it declares the object is refused. A
drop of a failed-over replication runs without `CASCADE` and keeps its tables. The provider never pauses, resumes or fails over an object.

A grant names a replication or a transfer by its directory and name
(`on_async_replication`, `on_transfer`), and GRANT and REVOKE name it by its
path. The planner puts a GRANT or a REVOKE after the creation or the change of
its object, and a drop replaces the REVOKE the host planned, since YDB drops an
object's access list with it and a REVOKE after the drop would name a path that
is gone. A change in place, such as a new lambda, keeps the entries.

Execution reads the object before the statement and refuses one that changed
since the plan read it, runs the statement once, and reads the object back.
YDB creates a replication's replica tables some time after the statement
returns, and refuses a view over a table that is not there yet. So a creation
whose replica a later step of the plan reads, such as a view over it, then
waits until each of its replica tables in this database exists, at most two
minutes. A table still missing then stops the run after the replication, with
the diagnostic `ydb.replica-tables-not-created`. A replication nothing in the
plan reads goes on at once.

The
statement names the object, the replica targets, the transfer's table and a
secret path absolutely; a transfer's topic keeps its relative spelling, because
25.1.4.7 cannot find a topic of its own database named absolutely (`Discovery
error: ... SCHEME_ERROR`). The TEXT form writes the host's statements, with
paths relative to the database root. A dev realm refuses both objects, which
reach past it.

On 25.1.4.7 with EnableTopicTransfer on, dropping a table a transfer wrote
into, after the transfer is gone, stops the server on a schemeshard VERIFY
(`AliveChildrenCount >= 1`); 26.2.1.14 drops it. The provider's CI therefore
keeps the flag at its default on 25.1 and checks the refusal there.

A consumer a transfer reads through is the transfer's. YDB makes one with a
generated name for a transfer that names none, and a declaration of the
changefeed or the topic cannot name it. The read marks each consumer a
transfer of the same read reads through (`Consumer.transfer` in the topic
description, which YDB never sets), and the comparison of the topic or the
changefeed keeps a marked consumer the declaration leaves out, as the host's
embedded engine adopts it: the run after a transfer's creation plans nothing,
and a change of the topic or the changefeed keeps the consumer. A topic
operation neither checks nor confirms a marked consumer, since the transfer's
drop, which the plan may run first, takes the one YDB made. A transfer read
outside the selected directories marks nothing.

A declared transfer whose table or topic the plan would not leave is refused
before anything is planned, in the host's embedded engine's words. The table
has to be declared, where the host lists every declared table among a
comparison's declared relations (`models.declared-relations`, contract 25.32):
YDB creates no transfer into a missing table (`Path does not exist`). A topic of
the transfer's own database has to be declared as a topic, where the host also
sends the transfer comparison the declared topics (`models.declared-objects`,
contract 25.33, ADR 0109 in Ptah), or be a changefeed of a declared table: YDB
accepts a transfer of a missing topic and then stops it (`Discovery error`).

A
table's changefeeds are a facet the row-table comparison owns; a host that
sends the transfer comparison the declared changefeeds of the tables
(`models.declared-facets`, contract 25.37, ADR 0119 in Ptah) has a stream of a
declared table accepted only when it is one of the table's declared
changefeeds, as the host's embedded engine did. A host that does not send them
leaves the comparison to take any stream of a declared table for a changefeed.

A topic or a changefeed the read recorded without describing it is one the plan
neither drops nor changes, so a transfer that reads it is compared, as the
embedded engine compared it: a topic the read lists and does not read, such as a
queue of the older persistent queue kind, and a stream of a declared table that
holds a changefeed no declaration can describe, such as one whose topic has a
consumer with a codec Ptah does not declare. The read leaves out the whole
changefeed set of such a table, so the comparison takes any stream of it for
one of its changefeeds. Only a host that sends the transfer comparison what the
read holds (`models.current-context`, contract 25.40, ADR 0122 in Ptah) says
so; without it such a transfer is refused like one of a missing topic.

A target without transfers refuses the transfer by the capability first, as the
embedded engine did.

Not modeled yet: HCL blocks.

`TestReplicationLifecycleSubprocess` creates a replication of the server's own
table, finds nothing to change, refuses a connection change while it runs,
makes it once the replication is paused, and drops it with its replica.
`TestReplicationFailedOverSubprocess` drops a failed-over replication and writes
to the table it kept. `TestTransferLifecycleSubprocess` creates a transfer from a
changefeed into a table, moves a row through it, changes its lambda and drops
it, and `TestTransferRefusedWithoutTheCapabilitySubprocess` checks the refusal on
a line without transfers. `TestHostAppliesAnAsyncReplication_HappyPath` and
`TestHostAppliesATransfer_HappyPath` drive `ptah schema apply` from YAML through
the same steps.

`TestHostGrantsOnAnAsyncReplication_HappyPath` and
`TestHostGrantsOnATransfer_HappyPath` grant a user a permission in the plan that
creates the object, keep it through a change in place and drop the object
without a REVOKE. Each needs a server whose gRPC services include replication.

## Resource pools and classifiers

The provider declares, compares, plans and applies resource pools and resource
pool classifiers, with the syntax, the rules and the statements of the host's
embedded YDB engine (`dialect/ydb/ydbworkload`). Both belong to the whole
database rather than to a directory, and YDB keeps them behind the
`EnableResourcePools` flag, off by default on every line: a session reports
`resource_pools` from the cluster's flags, so the address must name the
monitoring endpoint (see [Feature flags](connections-and-execution.md#feature-flags)).

A YAML document declares them under the document keys `resource_pools` and
`resource_pool_classifiers`, a Go file with the directives
`ptah:schema:resourcepool` and `ptah:schema:resourcepool:classifier`, and a YQL
schema with `CREATE RESOURCE POOL`, `CREATE RESOURCE POOL CLASSIFIER` and, for
the pool YDB creates with the database, `ALTER RESOURCE POOL default SET`:

```yaml
resource_pools:
  reporting:
    concurrent_query_limit: 10
    queue_size: 20
    query_memory_limit_percent_per_node: 25.5
  default:
    resource_weight: 30
resource_pool_classifiers:
  reporters:
    resource_pool: reporting
    member_name: analysts
    rank: 100
```

```go
//ptah:schema:resourcepool name="reporting" concurrent_query_limit="10" queue_size="20" query_memory_limit_percent_per_node="25.5"
//ptah:schema:resourcepool:classifier name="reporters" resource_pool="reporting" member_name="analysts" rank="100"
type Pools struct{}
```

A pool takes `concurrent_query_limit` and `queue_size` as whole numbers from 0,
and `database_load_cpu_threshold`, the three `*_percent_per_node` limits and
`resource_weight` as percentages from 0 to 100; a setting left out has no
limit, and zero is a limit. A queue needs a query limit or a load threshold
beside it, and the pool `default` takes neither of those. A classifier names
its pool and its rank, and its member when it matches one user or group.
Planning declarations without a read of the database, as `schema render` does,
refuses a classifier whose pool the declarations neither hold nor name
`default`: YDB takes such a classifier and runs the member's queries in
`default` without a word. A migration keeps a classifier routing to
a pool the plan drops, which YDB takes too. Each rule refuses with the host's
words.

A source declares the pools and classifiers it manages, not every one of the
database, so the comparison keeps one a source leaves out, as the host does;
an absence a source claims for one object is left undecided. A pool compares
setting by setting and a classifier as a whole. The `default` pool must be read
before its settings change, and it is never created or dropped. A schema
written as SQL, such as `schema inspect --format sql`, writes it as the `ALTER
RESOURCE POOL default SET` of the settings it names, and writes nothing for it
when it names none, as the host's engine did.

The planner writes each workload statement of a plan as a step of its own,
outside a transaction, ordered after the steps that create or change a role and
before those that drop one, since a classifier names one. The steps are chained
in the host's order: classifier removals, pool removals, pool creations and
changes, classifier changes, then classifier creations. YDB keeps one
classifier per rank and checks it at every statement, so a classifier that
moves to a rank another changed classifier leaves is dropped and created again
around the move, in two steps.

Each step runs after the common steps that need
neither it nor a later workload step, which is where the YDB engine built into
the host at 74de0b65b ran its workload statements: a pool right before the
first grant on it, and a classifier after every common step. Measured on
25.1.4.7 and 26.2.1.14 with a group, a table, a topic, a pool, its classifier
and grants on the table, the topic and the pool, `schema apply` and
`migrations generate` create the pool after the table, then the grants, then
the classifier, and the down file drops the topic, the table, the classifier,
the pool and the group. In a migration file each step is one statement.

Execution of a step reads `.sys/resource_pools` and
`.sys/resource_pool_classifiers` before its statement and refuses an object
that changed since the plan read it; it runs the statement once and reads both
views back. The workload
manager reads its classifiers from a cache, so for a while after a classifier
and its pool are dropped it can still route a query to the pool, and YDB
refuses that query with `Failed to resolve pool id` before it runs it. A read of
the views refused so is sent again, for up to 20 seconds, for an inspection and
for an execution's check and confirmation alike; a statement is never sent
twice. A classifier with no member name routes every query of the database,
the server's own included.

Measured on 25.1.4.7: while such a classifier
routed every query to a pool that admits none, a read of
`.sys/resource_pool_classifiers` sent to the `default` pool ended in
`System view: timeout`. Inspection reads
the views on a target with resource pools, for a read of the whole database,
one that selects the root, `/`, and one filtered to other directories where the
host lists `schema.database-kinds`: the engine names both kinds database
kinds, so a filtered read reports them beside its scope, named at the database
(ADR 0065). So `schema apply --schemas app` creates a pool its source
declares, and keeps the database's pools when the source declares none, as a
run of the whole database does.

With a host that does not list the feature, a
read filtered to other directories cannot report what belongs to the database,
so it leaves both kinds uninspected; selecting the root as well (`--schemas
app,/`) reads them. A dev realm holds none of its own.

A complete workload read records absence inside the opened database's root
namespace, including when no pools or classifiers were returned. It establishes
no absence in another database or a directory namespace. The host preserves
that boundary when it compares a new declaration against the inspection.

`TestHostAppliesAResourcePoolInARunOfADirectory_HappyPath` drives `ptah schema
apply --schemas` the fixture directory alone: it creates a declared pool, reads
it as synced, keeps it when the source leaves it out, and changes it in place.

`TestWorkloadLifecycleSubprocess` creates a pool and two classifiers, swaps
their ranks from the text of a migration file, refuses a change planned before
another writer changed the pool, and removes them, reading the views back after
each step. `TestHostAppliesResourcePools_HappyPath` drives `ptah schema apply`
over the fixture directory and the root from a YAML, a Go and a YQL source: it
creates them, finds nothing to change, changes a pool and swaps two ranks,
finds nothing to change again, and keeps them when the source leaves them out. `TestHostCreatesAClassifierAfterTheGrants_HappyPath` pins the order of
`schema apply --dry-run` and of the up and down files of `migrations generate`.
