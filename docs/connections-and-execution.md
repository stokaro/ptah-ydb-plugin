---
title: YDB connections and execution
description: Connection addresses, dev realms, transaction behavior, migration splitting, and text units.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How does the YDB provider connect and execute work?"
goal: "Understand YDB connections and the provider execution path."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 6
---

# YDB connections and execution

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Connection addresses

Use `ydb://host:2136/database` or `ydbs://host:2135/database` for TLS. The
database comes from the path or from the `database` parameter; naming it twice
is refused when the two differ.

The accepted parameters follow the host's embedded engine:

- `database`, `token` and `use_env_credentials` choose the database and the
  credential. A token, URL user/password and `use_env_credentials` cannot be
  combined. `use_env_credentials` reads the `YDB_*` variables of the provider
  process, service-account keys and instance metadata included, and fails when
  none is set, so an environment that would connect anonymously is refused. An
  address with no credential source connects anonymously.
- `go_balancer` and the older `balancer` choose the balancer; naming both is
  refused, and so is a value the SDK does not know. `go_default_idempotent`
  takes a boolean and `prefetch_query_result_parts` a non-negative integer.
  The SDK reads these three.
- `go_query_mode` and `query_mode` accept `query` only. `go_fake_tx` and
  `go_query_bind` are refused by name, because a faked transaction would read
  outside the snapshot read and the SDK's binders rewrite a `?` inside a YQL
  literal.
- `monitoring` names the cluster's monitoring endpoint, an `http://` or
  `https://` address with no user, path, query or fragment. A session reads
  the database's feature flags there when it opens (see "Feature flags"), and
  inspection reads the description of each column table there. Both reads
  present the connection's own credential in YDB's bare `Authorization`
  header, follow no redirect and read at most 4 MiB. A credential that travels
  over TLS (`ydbs://`) is never sent to a plain `http://` endpoint: the session
  is refused and asked for `https://`.

Unknown or repeated parameters, conflicting database paths, and malformed
values are refused, and a refusal names the parameter and never its value. Two
refusals are stricter than the host's: a fragment, and a query the host reads
as empty because it cannot unescape it. The `testdata/oracle` corpus beside
`internal/connection` holds the host's answer for each address it covers.
Credentials travel in the session-open request and are omitted from receipts,
SDK addresses, and error diagnostics. Native errors currently report only the
failed operation, which limits diagnosis until structured redaction is added.

### Feature flags

A release line runs with default feature flags, and an operator can turn a
flag on or off, so one cluster can do more or less than its line's defaults
say. When the address names a monitoring endpoint, opening a session reads
`/viewer/json/feature_flags?database=<db>` and folds the flags that gate a
capability the provider owns into the session's capabilities, `false`
included. A failed read fails the session rather than falling back to the
defaults, as in the host's embedded engine. The refusal is a reason the host
prints, and it names the right a user lacks: on 26.x the page is served only
to a user with `DESCRIBE SCHEMA` on the database. What the endpoint answered
stays out of the reason. Without the endpoint the session reports the
defaults of every supported line.

- `tiered_ttl` uses `EnableTieringInColumnShard`; default: `false` on every line from 25.1 to 26.2.
- `changefeed_topic_auto_partitioning` uses `EnableTopicAutopartitioningForCDC`; default: `false` on 25.1, `true` from 25.2.
- `external_data_sources` uses `EnableExternalDataSources`; default: `false` on every line from 25.1 to 26.2.
- `external_object_replace` uses `EnableReplaceIfExistsForExternalEntities`; default: `false` on every line from 25.1 to 26.2.
- `resource_pools` uses `EnableResourcePools`; default: `false` on every line from 25.1 to 26.2.
- `secrets` uses `EnableSchemaSecrets`; default: `false` through 25.3, `true` from 25.4.
- `vector_indexes` uses `EnableVectorIndex`; default: `false` on 25.1, `true` from 25.2.
- `full_text_indexes` uses `EnableFulltextIndex`; default: `false` through 26.1, `true` from 26.2.
- `local_bloom_indexes` uses `EnableLocalBloomFilterIndex`; default: `false` through 26.1, `true` from 26.2.
- `local_ngram_indexes` uses `EnableLocalBloomNgramFilterIndex`; default: `false` through 26.1, `true` from 26.2.
- `unique_index_on_existing_table` uses `EnableAddUniqueIndex`; default: `false` on every line; 25.1 and 25.2 do not list the flag.
- `index_rename` uses `EnableMoveIndex`; default: `true` on every line from 25.1 to 26.2, for a host that lists `planning.index-renames`.
- `local_min_max_indexes` uses `EnableLocalMinMaxIndex`; default: `false` on every line from 25.1 to 26.2.
- `column_family_cache_mode` uses `EnableTableCacheModes`; default: `false` through 25.3, `true` from 25.4; 25.1 and 25.2 do not list the flag.

The release line alone decides the other vector capabilities, as the host's
line presets name them: `vector_index_maintained_on_write` from 25.3 (an index
built on 25.1 or 25.2 does not find the rows written after its build), and
`vector_bit_type` from 26.1 (25.1 to 25.4 do not build an index over bit
vectors over rows: `bit vector type is not supported`, `Unsupported
vector_type: VECTOR_TYPE_BIT`).

The host maps more flags than these. The provider reports only the
capabilities it plans with, because the host plans with the others itself.
`index_rename` is `false` for a host that does not list
`planning.index-renames`: such a host has no step to send a renamed index
with, and would refuse the plan. `internal/featureflags/testdata`
holds the pages the mapping is checked against, and a live test turns the
flag on and off through the server's dynamic configuration and reads the
capability each new session reports.

A plan refused for want of a capability a flag decides names the flag, after
the host's words for the capability: `...; YDB keeps it behind feature flag
<Flag>: turn the flag on, and name the cluster's monitoring endpoint in the
URL (monitoring=http://host:8765) so Ptah reads the flags before it plans`. A
session without the endpoint takes its release line's default, so the flag
alone does not change the answer.

Session close releases both the SQL pool and its parent native driver, even
when the RPC context is canceled. Cleanup results are retained for repeated
closes. A process accepts at most 1,024 lifetime session handles; start a new
provider process when that bound is reached.

## Dev realms

Database context is resolved without connecting or reading credentials. A dev
realm carries both its selected root and the authored database root. Context
is captured separately from object identity and survives normalization,
conversion, and saved operations. Absolute secret and external data source
references require this context before selection or planning. Unresolved
references retain an explicit pending state; they cannot report an empty or
partial dependency list.

SQL cannot create a YDB database, so a dev or scratch database on a server the
run does not own is a directory. `dev_realm=<name>` on an address makes the
provider treat `<database>/ptah_dev/<name>` as the whole database of the
session. A name is 1 to 64 lowercase letters, digits and underscores, and the
address must name the database that holds the realm. Opening such a session
creates the directory when it does not exist.

The engine declares `database.dev-realms`. A host that lists the feature sends
a dev database request with the operator's address instead of a spec, and the
provider creates a realm of the run's own in the database the address names, as
the host's embedded engine does: a name of 16 random hexadecimal digits, and an
address that keeps the operator's scheme, host and credentials and names the
realm in place of any it named; an address whose `dev_realm` is not a realm
name is refused, as any address that is not a YDB address is. Releasing the lease removes the realm with
everything in it, and then `ptah_dev` when no realm is left in it; an object
there is no statement to drop for stops the removal before anything is dropped.

The provider also removes the realms it holds when its process ends. A realm is
a directory, which YDB creates only for a user who holds
`ydb.granular.create_directory`; a user refused the directory is refused the
realm with that right named.

The session receipt reports the realm's path as its database, and inspection,
locks, migration history, migration metadata and every native write resolve
against it, so the realm reads as an empty database. Every SQL unit starts with
`PRAGMA TablePathPrefix` naming the realm, which resolves a relative path inside
it. The pragma does not confine an absolute path, a path that climbs out with
`..`, a name written through a `$` expression, or an object the whole database
shares, so the provider reads each unit before it dispatches any of them and
refuses the batch with `ydb.realm-escape` when a statement writes outside the
realm, moves the prefix, runs code it computes, or creates a user, group,
permission, resource pool or backup.

An external data source or table, an async
replication, a transfer and a streaming query are refused too. A statement the
guard does not recognize is refused. The rules are the host's replay guard for a
realm; `internal/yql/testdata/realm` holds the host's answers for 94 statements,
and `--!syntax_v1` is the one difference.

The provider also serves the replay guard itself (`database.dev-replay-guard`):
the host asks, without a session, about each statement it is about to replay
into a dev database, and the provider answers as the host's YDB guard does
(`internal/devclean/ydbreplay`). A run that owns the whole server, because the
operator declared it disposable or the run provisioned it, may change anything
on it but what reaches past it: an external data source or table, an async
replication, a transfer and a streaming query, and a translation setting, which
changes how the rest of the text is read. Every other scope confines a statement
to the dev database, with the rules above, and refuses `--!syntax_v1` as the
host does. `testdata/realm/scopes.json` holds the host's verdict on each
statement under every scope.

## Dev database cleanup

The provider lists and empties the dev database a session selected
(`database.dev-cleanup`), as the host's embedded engine lists and resets one.
The list and the cleanup read one walk of the session's root, each directory's
entries in name order and a directory after its contents, so a database the list
finds empty is one the cleanup leaves as it is. The walk leaves alone a name
that starts with a dot, which belongs to the server, the lock node the session
keeps its locks on, and at the root of a database the directory of the dev
realms. A request that names schemas walks only those directories, and keeps
them.

The cleanup drops every transfer and streaming query, then every async
replication, then external tables and column tables, then external data sources,
then the other objects, then secrets, and removes the directories, deepest
first, with the host's statements. A coordination node is dropped through the
coordination service. An object there is no statement for, such as a resource
pool, refuses the cleanup before anything is dropped, and the reply names it. A
cleanup that stops after its first step is `UNKNOWN`, and the host treats the
database as not clean.

A session on a dev realm takes its locks on a `ptah_locks` node at the realm's
root, and the cleanup of the realm keeps that node, where the host's embedded
engine dropped a node there. That engine took a realm session's locks on the
node at the database root instead (`path.Join(driver.Name(), "ptah_locks")` in
its `internal/dblockext/ydb`; measured on 25.1.4.7, a realm run creates the
root's node), so the realm's node was no lock of its, and it created the root's
node from inside a realm, which the realm's guard refuses for every other
write. The provider keeps a realm session inside its realm: its lock node is
there, and the read and the cleanup leave it alone as they leave the database
root's node alone.

`internal/devclean/testdata` holds scheme trees and what the host's embedded
engine listed, dropped and removed for each, recorded by an oracle program; the
tests replay each tree through the provider's walk and cleanup.

A session on the database itself leaves the `ptah_dev` directory out of
everything it reads, and a table owner may not be named `ptah_dev`.

## Dev databases

The provider advertises `database.dev`. A host request with the spec
`docker://ydb[/<tag>[/local]]` starts `ydbplatform/local-ydb:<tag>` (`latest`
without a tag) with in-memory disks, publishes its port on loopback only, waits
until the server takes DDL, and returns a lease and the address
`ydb://127.0.0.1:<port>/local?go_balancer=disable`. The host removes the
container by releasing the lease; the provider also removes every lease it still
holds when it receives SIGTERM or SIGHUP or when the host closes it. A provider
killed outright leaves its containers, which carry the label `ptah-dev` for
manual removal.

The spec follows the host's `docker://ydb` rules: the engine is matched as
written, one path segment is a tag, and local-ydb serves the database `local`
alone. The host also sends the image form the operator types,
`docker+ydb://[<host>/]<image>[:<tag>][/local]`, which starts the image it
names and is read as the host reads the image form of its other engines: the
last path segment is the database when there are two or more and it holds no
colon, the segments before it are the image, a host other than `_` leads the
image, and an image with no tag is `latest`. The image has to serve the
database `local` without a credential, as local-ydb does. Parameters are merged over the default `go_balancer=disable`.

Those that
would point the connection elsewhere or hand the container a credential
(`host`, `hostaddr`, `port`, `user`, `password`, `dbname`, `passfile`, `service`,
`servicefile`, `database`, `dev_realm`, `monitoring`, `token`,
`use_env_credentials`) are refused, and so is any parameter a YDB address does not accept, before a container
starts. The host starts other engines itself.

The provider drives the `docker` command line, which it finds on `PATH`. The
host starts the provider with an explicit environment, so pass `PATH`, and
`DOCKER_HOST` or `DOCKER_CONTEXT` when the daemon is not the default. local-ydb
takes every connection without a credential, so a daemon on another machine is
refused (`UNSUPPORTED`), and a missing command or an unreachable daemon is
`UNAVAILABLE`.

## Execution and transactions

Execution sends native SQL units through YDB's query service. A batch is checked
before its first unit runs. Unsupported native envelopes refuse the whole batch.
SQL units use a pinned connection; native coordination, table creation and table alteration units
use their native services. Dispatched writes are not retried, except a
changefeed statement YDB refused because another schema operation held the
table's path (see [Changefeeds](native-objects.md#changefeeds)).
Request IDs correlate calls and do not provide exactly-once execution.

Results distinguish a confirmed prefix from an uncertain remainder. A
dispatched unit YDB refused is `NOT_APPLIED`, or `PARTIAL` after a confirmed
prefix, when nothing of it can have been applied. That takes a status that
establishes the refusal (`BAD_REQUEST`, `SCHEME_ERROR`, `PRECONDITION_FAILED`,
`ALREADY_EXISTS`, `NOT_FOUND`, `UNSUPPORTED`, `ABORTED`, or `GENERIC_ERROR` for
a plain data statement, for one `ANALYZE` statement, which changes no schema
object and no row, or for a text YDB refused while compiling it, which its
issues show by naming a position in the text and no execution stage) and a
unit that ran in a caller transaction, which YDB
applies only by its commit, or that is one query YDB applies whole: a data
query or one scheme statement.

A native unit whose first statement YDB refused
is judged by that statement: the service reads and checks before it writes, and
writes only through its statements, so nothing of the unit was applied. That
holds for every native unit but a column inventory, which alters a table through
the table service first, a rebuild's copy, a table creation or alteration and a
coordination node operation, which run no statement. Its message is YDB's own
issue text on one line, which no host prints; its reason is the provider's (see
[Error reasons](catalog-and-errors.md#error-reasons)).

Every other failed dispatched unit reports `UNKNOWN`, even when earlier units
succeeded: a block or action call that runs scheme statements, a `BATCH`
statement, a unit of several scheme statements, a native unit past its first
statement, a unit that holds a coordination node statement, a unit that expands
secret inputs, and a
failure that carries no server status. A unit that holds a comment statement
runs as its statements one after another, and reads by the first one when that
one fails, since nothing of the unit ran before it: a query by the rules above,
and a comment statement, one `AlterTable` YDB applies whole, by a status that
establishes the refusal, such as `BAD_REQUEST` for attributes past YDB's
budget. A later one failing leaves the unit `UNKNOWN`.

Cancellation between
units reports the confirmed prefix. Diagnostics omit raw driver errors.

A unit YDB refused in a caller transaction ends the transaction: measured on
26.2.1.14, YDB has dropped it, and a rollback answers `Transaction not found`.
The provider rolls it back and retains `ROLLED_BACK` for the caller's finish.
A commit YDB answered `ABORTED` applied nothing and is `ROLLED_BACK`.

A unit refused before dispatch reports `NOT_STARTED`, or `PARTIAL` after an
applied prefix, with the diagnostic `ydb.precondition-refused`. Where the
reason is the provider's own text about the object, such as a plan the object
changed after, a path another object holds, or a read the server answered with
a status other than success, the message is that reason, naming the object by
its path and the server's status by its name. Any other reason, such as a lost
lock or a failed system-view query, gets a fixed sentence, so no server text,
credential or declared value reaches the host.

Every native creation reads where its object goes before the write: the
listing of its directory, or the system view for a streaming query and the
resource pools. YDB creates the directories an object needs with the object,
so a directory that does not exist holds no occupant, by the rule a filtered
read uses (see [Schema observations](catalog-and-errors.md#schema-observations)), and the first
object of a new directory is created there.

On 26.2.1.14, YDB created a topic,
a coordination node, a secret, an external data source and an external table,
a row table (through YQL and through native CreateTable), a column table, a
view and a streaming query two directory levels below an existing directory;
25.1.4.7 did the same for every kind but secrets and streaming queries, which
it does not keep. Resource pools and classifiers have no directory.

The provider offers `units.statement-timeout`. A host that lists it sends a
migration's statement timeout with each Execute call, and the provider bounds
each unit with it and answers for what YDB does when it fires, as the host's
embedded engine does (`migration/migrator`, `ydb_timeouts.go`); the reply says
the timeout stopped the unit. A unit in the caller's transaction is canceled
and the transaction never commits, so nothing of the unit is applied
(`NOT_APPLIED`, or `PARTIAL` after a prefix), and the provider rolls the
transaction back.

An index added to a table that exists, and a column added
with a default, run as a build that keeps running when the client stops
waiting: the provider cancels the build through the operation service and
waits for it to end, so a stopped build is not applied, and one that completed
first counts as applied. Any other unit, and a build that cannot be found or
does not end, may still be applied by the server, and its outcome is `UNKNOWN`.
A host that does not list the feature bounds a statement with the deadline of
the Execute call, and a unit that deadline stops is `UNKNOWN` and is not sent
again. The session receipt reports `migration_statement_timeout=true` either way.

Transaction handles belong to one provider session. DEFAULT and SERIALIZABLE
both use YDB's native serializable read/write mode; receipts retain the
requested isolation. Begin cancellation remains connected while the database
transaction opens, then detaches when the handle is returned. Ending the begin
RPC therefore does not roll back later work.

A session permits one active transaction. Execution must name its handle while
it is active. Commit and rollback retain their terminal result, including
UNKNOWN and an aborted commit, without issuing a second database operation. Rollback of an unused
handle fences a late begin. The SDK client sends rollback after a failed begin,
including a duplicate begin; callers must not reuse handles.
Each session retains at most 1,024 transaction handles.

Closing a session rolls back unfinished work even if the close caller was
canceled. Connection acquisition for begin and native transaction calls have a
30-second bound. Unconfirmed cleanup remains an error. YDB schema changes are not transactional; session
receipts explicitly report `transactional_ddl=false`. Live tests verify that
DDL and SQL transaction-control statements cannot escape an explicit data
transaction. See [YDB transaction semantics](https://ydb.tech/docs/en/concepts/transactions).

Coordination, external object, row-table creation, row-table alteration,
changefeed and topic operations use native envelopes, or YQL statements in the
TEXT form; see [Text units](#text-units). Other native operations are described
in the [object lifecycle reference](native-objects.md). A SQL unit that runs one of the host's
coordination node statements is run through the coordination service; the
host's extended comment statements are not implemented here.

## Migration splits

The provider and the ydb engine list `units.migration-split` (contract 25.12,
ADR 0054 in Ptah) beside the `sessions.transactions` and `sessions.history`
services it requires. `SplitMigration` reads one migration body offline and
returns the queries YDB runs it as, cut by `internal/yqlquery` as the host's
embedded YDB engine cuts them: each scheme statement and each `BATCH`
statement a query of its own, each run of data statements one query, and
every `PRAGMA`, `DECLARE`, named expression and action carried into each later
query. A unit's `sql` is that query's text, and its `source` is the query's own
statements as written, which the host hashes and records. The corpus under
`internal/yqlquery/testdata/corpus` holds the host's text and source for each
case, recorded by `internal/rowdata/testdata/oracle`.

A data query is `WITH_CHECKPOINT`: the host runs it in a serializable
transaction together with the history write that records it. A scheme query,
a `BATCH` query, and a block or action call that runs a scheme statement are
`OUTSIDE`, and the host runs each with no transaction. A block or an
`EVALUATE FOR` that runs only data statements is a data query: measured on
26.2.1.14, YDB runs and commits `DO BEGIN UPSERT ...; END DO`, `DO $a(1)` over a
data action and `EVALUATE FOR ... DO BEGIN UPSERT ...; END DO` in an
interactive serializable transaction, and refuses one that runs a scheme
statement there.

The split refuses the whole body, with the byte range of the refused text and
a reason that quotes nothing from the body, where the host's embedded engine
refuses it before a run: a translation setting other than `--!syntax_v1` (such
as `--!ansi_lexer`), a client delimiter directive (`DELIMITER` or
`-- atlas:delimiter`), a statement that controls a transaction (such as
`COMMIT`), and a block or action call that runs a scheme statement and a
statement that reads or writes a table, which YDB refuses whole. Each check
assertion gets the verdict `RunStatement` gives the same text under
`READ_ONLY` before it opens anything, so a dry run refuses what the run
refuses. The run still proves the reads local against the catalog.

For a host that lists the feature, a unit YDB aborted for a conflict in a
caller transaction (`ABORTED`, `Transaction locks invalidated`) is
`NOT_APPLIED` with `Execution.transaction_aborted`, and a commit YDB aborted is
`ROLLED_BACK` with `TransactionResult.aborted`. Neither applied anything, and
the host may run the transaction again. A host that does not list the feature
gets the same outcomes without the flags. An `ABORTED` answer YDB gives for the
transaction itself, not for a conflict, sets neither flag, since every new
attempt answers the same: measured on 25.1.4.7, a transaction whose writes do
not fit one datashard program, such as the copy of a table rebuild over about
48 MiB, commits with issue 200509 (`Datashard program size limit exceeded`),
and the commit is `ROLLED_BACK` with the reason that says so.

## Text units

The provider lists `render.text-units` (contract 25.3, ADR 0034 in Ptah). A
host that lists it asks for the TEXT form where it writes text: migration
files, `schema render`, SQL diffs and SQL inspection. Every unit is then a YQL
statement, the one the host's embedded engine writes for the same change, and
this provider's `Execute` does with it what the native call does:

| Operation | Statement |
| --- | --- |
| Row-table creation | `CREATE TABLE` with its columns, key, families, TTL, automatic partitioning, read replicas, key bloom filter and starting layout, then `ALTER SEQUENCE ... RESTART WITH` for a Serial whose sequence the declaration gives a start or an increment |
| Serial sequence change | `ALTER SEQUENCE` on the sequence's absolute path, with `START WITH` and `INCREMENT BY` and no restart |
| Row-table alteration | One `ALTER TABLE`: families and placements, added or dropped columns, `SET (TTL = ...)` or `RESET (TTL)`, the whole automatic partitioning set, or `ADD`/`DROP INDEX` |
| Column table | The `CREATE TABLE ... STORE = COLUMN` and TTL statements the provider runs |
| Coordination node | `CREATE COORDINATION NODE ... WITH (...)`, `ALTER ... SET (...)` naming the settings that change, `DROP COORDINATION NODE` |
| External data source or external table | `CREATE`, `CREATE OR REPLACE` or `DROP EXTERNAL DATA SOURCE` or `EXTERNAL TABLE`, with the paths and options as the declaration writes them |
| Topic | `CREATE TOPIC` with its consumers and the settings the declaration states, `DROP TOPIC`, or the `ALTER TOPIC` statements of an alteration, one to a line: the statements `Execute` runs natively for the change |
| Changefeed change | One statement to a line: `ALTER TABLE ... DROP CHANGEFEED` for each removal, then `ALTER TABLE ... ADD CHANGEFEED` for each addition with one `ALTER TOPIC ... ADD CONSUMER` per consumer, then the `ALTER TOPIC` statements that change a topic in place |

YQL has no coordination node statement, so `Execute` reads one as the host's
YDB connection does and runs it through the coordination service: alone in its
query after nothing but definitions, with the query's `PRAGMA TablePathPrefix`,
and inside a dev realm relative to the realm. A CREATE checks first that no node
is at the path, because the service answers a second creation with success and
keeps the node as it is; an ALTER merges its settings with the node's and
refuses a result YDB would move into its range. Such a statement in a
transaction, or one that cannot be read, refuses the batch before any unit runs.

A change only the native call makes refuses the batch with `NO_TEXT_FORM` and
no unit: user attributes other than Ptah's comments, storage settings, tiering, the partition columns, a
family kept in memory, a default of a type without a literal, a sequence on a
column that is no Int16, Int32 or Int64, a start or an increment on a 16-bit or
32-bit Serial's sequence, a sequence of a creation or a change that names no
database (ALTER SEQUENCE takes only the absolute path), a TTL run interval other than YDB's own, a coordination node that
declares its access, an external data source or external table dropped and
created again around the plan's other statements, which the native call gives
the owner and the access entries of the old one, and every topic
operation. A host that does not list the feature gets native units.
