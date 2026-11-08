---
title: Migrations on YDB
description: How the provider splits a YDB migration into queries, holds the migration lock and applies statement timeouts.
type: concept
audience:
  - database-engineer
  - ci-operator
readerQuestion: "How does a migration run on YDB, and what do the lock and the timeouts do there?"
goal: "Predict how a YDB migration is split, locked and timed out, and resume one that stopped."
sourceOfTruth:
  - internal/migrationunits
  - internal/locking
  - internal/history
  - stokaro/ptah#4204
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 4
---

# Migrations on YDB

`ptah migrations up`, `down` and `baseline` run a YDB migration through this
provider. Ptah's [apply reference](https://docs.ptah.run/edge/versioned/apply/)
describes the run for every engine; this page says what is particular to YDB:
how a migration is split into the queries YDB runs, the migration lock, and the
statement and lock timeouts.

## Queries

YDB runs a schema statement only outside a transaction, never in the same query
as a data statement, and a named expression, an action, a `DECLARE` or a
`PRAGMA` exists only in the query that holds it. The YDB provider therefore
lists `units.migration-split` and splits a YDB migration into a sequence of
queries, which Ptah runs, counts and records:

- Each schema statement — `CREATE`, `ALTER`, `DROP`, `GRANT`, `REVOKE`,
  `TRUNCATE` and the rest — is a query of its own, in file order. So is a block
  or an action call that runs one: `DO BEGIN ... END DO` holding a schema
  statement, or `DO $action()` and `EVALUATE FOR ... DO $action($x)` over an
  action whose body holds one. So is Ptah's own `COMMENT ON`, which the
  provider runs through YDB's table service.
- Each `BATCH UPDATE` and `BATCH DELETE` is a query of its own, outside any
  transaction, which is the only way YDB runs one. YDB applies it in batches
  rather than atomically, so an interrupted one leaves its outcome unknown, as
  an interrupted schema query does.
- Consecutive data statements are one query. It runs in a serializable
  transaction together with the revision checkpoint that records it, and a
  transaction YDB aborts for a conflict runs again.
- A definition — `PRAGMA`, `DECLARE`, `$name = ...`, `DEFINE ACTION`,
  `DEFINE SUBQUERY` — is carried into every query after it, so each query sees
  what the file defined above it.
- A `--!syntax_v1` line at the head of the file heads every query.

In this file the `UPDATE` uses `$name` across the `ALTER TABLE`, which runs as
a query of its own:

```sql
CREATE TABLE `app/items` (id Int64 NOT NULL, name Utf8, PRIMARY KEY (id));
$name = 'first'u;
UPSERT INTO `app/items` (id, name) VALUES (1l, $name);
ALTER TABLE `app/items` ADD COLUMN note Utf8;
UPDATE `app/items` SET note = $name WHERE id = 1l;
```

It runs as four queries: the `CREATE TABLE`, the definition with the `UPSERT`,
the definition with the `ALTER TABLE`, and the definition with the `UPDATE`.

YDB 26.2 fails an `UPSERT` on a table with a unique index when the statement
names no column that a synchronous global index is keyed on. The server answers
`INTERNAL_ERROR` with
`verification=!hasUniqIndex || !usedIndexes.empty();fline=kqp_opt_phy_upsert_index.cpp:359`
and writes nothing. This is a defect in the server: 25.1 runs the same
statement. Ptah reports the query as refused, with the reason, and the run
stops there. Name every column of the table in such an `UPSERT`, or use
`UPDATE` or `INSERT`, which 26.2 runs.

The queries are what the revision counts, what partial hashes digest and what a
resume skips. Progress is recorded after each query. An interrupted schema query
leaves its outcome unknown, as an interrupted statement does in a file whose
transaction mode is `none`, and the row has to be inspected and repaired. A data
query commits together with its checkpoint, so the revision row records it
exactly when it committed. A failed run never records fewer queries than the row
holds, even when the answer to a commit was lost, and when the row cannot be
read the run writes nothing to it. `up --allow-dirty` then resumes at the first query the row does not record.

Some files cannot be split the way YDB reads them, and the provider refuses them
before the run writes anything:

| In a YDB migration file | Why it is refused |
| --- | --- |
| `DELIMITER` or `-- atlas:delimiter` | YQL has no client delimiter; every statement ends with a semicolon |
| A `--!` setting other than `--!syntax_v1` | `--!ansi_lexer` changes how the text is read, and YDB refuses `--!syntax_pg`, `--!syntax_v0` and unknown settings |
| `COMMIT`, `ROLLBACK` or `BEGIN` | YDB refuses `COMMIT` inside a query, and Ptah decides where each transaction begins and ends |
| A `DO` block, an action call or an `EVALUATE` loop that runs a schema statement and a statement that reads or writes a table | YDB refuses such a query whole (`Queries with mixed data and scheme operations are not supported`), and the schema statements before it would already be applied |

[Migration bodies the provider splits](https://docs.ptah.run/edge/extend/executable-providers/#migration-bodies-the-provider-splits)
shows how such a refusal reads, and
[Migration splits](../README.md#migration-splits)
gives the provider's rules in full.

The provider keeps the revision table, the migration log and the tag table, in
the directory `--migrations-schema` names or at the database root; see
[Migration history](../README.md#migration-history). A database Ptah's
built-in YDB engine migrated keeps its tables: the provider reads and writes
that engine's `schema_migrations`, `schema_migrations_log`,
`ptah_migration_tags` and `atlas_schema_revisions` in their own layout, so
`migrations status`, `up`, `down`, `--allow-dirty` resumes, tags and
`ptah-compat migrate apply` continue where that engine stopped, and the
engine can still read the tables.

A primary key change, a column type change or `SET NOT NULL` is planned only as
a table rebuild, and only when `migrations generate` is given
`--allow-table-rebuild`; see
[Native table rebuild](../README.md#native-table-rebuild).
Each step of the rebuild is a query of its own, so a run interrupted between two
of them resumes at the next with `up --allow-dirty`.

## Statement timeouts

YDB has no statement timeout a session can set, and a schema statement ignores
the operation timeout of a request. So `--statement-timeout`, the
`statement_timeout` directive and `migration.statement_timeout` reach the
provider with each query of the migration, and the provider puts the deadline on
the query. What happens when it fires depends on the query:

| Query | When the deadline fires |
| --- | --- |
| Data statements | YDB cancels the query. Its transaction holds the revision checkpoint and never commits, so nothing is applied and the revision records the query as not run. |
| `ALTER TABLE ... ADD INDEX`, or `ADD COLUMN` with a default | YDB runs these as builds, which go on after the client stops waiting. The provider cancels the build through the operation service and waits for it to end. A canceled build leaves no index and no column, and the revision records the query as not run. |
| Any other schema statement, or a build that cannot be found or does not end | YDB keeps running it and cannot cancel it. The run stops, and the revision records the outcome as unknown: inspect the table, then repair the revision. |

In each case the migration fails and the revision records which of these
happened. A rerun with `up --allow-dirty` and a larger timeout, or none,
resumes at the query that was stopped.

`--lock-timeout`, the `lock_timeout` directive and `migration.lock_timeout` are
refused on YDB. No statement there waits for a lock: a schema statement on a
table under another schema operation fails at once with `has been locked by
tx`, and a schema change made while a transaction holds locks on the table
succeeds at once and aborts that transaction at its commit.

## The migration lock

YQL has no advisory lock, so the provider takes Ptah's migration lock as a
semaphore on the coordination node `ptah_locks` at the database root, one
semaphore per lock name. It is exclusive and ephemeral: the server releases it
when the session that holds it ends, whether the run released it or died. The
provider creates a missing node with strict consistency, makes strict the node
Ptah's built-in YDB engine created with neither mode set, and refuses a node
someone set otherwise, as [Coordination locks](../README.md#coordination-locks)
describes. The node holds no data, so dropping it while no run is active is
safe, and the next run creates it again. The provider's read leaves it out, so a
plan never drops it. Like every advisory lock, it keeps Ptah runs apart; it does
not stop a client that ignores it.

The semaphore is held by a coordination session of its own, not by the
connection the migrations run on, so the server can take it away while the run
goes on. When the provider reports the lock lost, the statement running then is
canceled, no further statement starts and no data query commits, nothing more
is written to the revision table, and the run fails with the loss. The revision
table then records what the run committed, and a resume starts there. See
[Coordination locks](../README.md#coordination-locks)
for the session and its keepalives.
