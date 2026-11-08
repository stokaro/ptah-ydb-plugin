---
title: YDB migration state and common objects
description: Reverse migrations, common objects, history, journals, tags, and coordination locks.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How does the YDB provider retain migration state and coordinate changes?"
goal: "Understand YDB migration state, locks, and common objects."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 7
---

# YDB migration state and common objects

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Reverse migrations

The provider lists `models.reverse` (contract 25.19, ADR 0062 in Ptah) and
declares one reversal service, `ydb-objects`, for the changes of its
standalone objects: coordination nodes, secrets, topics, streaming queries,
external data sources and external tables, resource pools and classifiers,
async replications and transfers. `ptah migrations generate` writes the down
file of such a change from this service's reverse of it, as the host's
embedded engine writes it from its own:

- The reverse asks for the declaration of what the forward change found, the
  conversion of the read, and finds what the forward change leaves, the
  conversion of its declaration. A creation reverses to a drop, a drop to a
  creation and a change to the settings the object held. The prediction of
  what the forward change leaves is the forward state the host plans the
  reverse against; it is never read from the server.
- The down file holds the reverse's statements, in the exact statement style
  the host plans in, and a `-- Recovery limit:` line for what the rollback
  cannot undo, in the embedded engine's words: a dropped topic loses its
  messages and its consumers' positions, a dropped coordination node its
  semaphores, a restored workload setting cannot undo the queries run under
  the forward one.
- A rotated secret has nothing to run in reverse: the value it replaced was
  never read. A secret created again takes the value of the variable for its
  path when the rollback runs.
- A changed streaming query text keeps `allow_state_reset` in the reverse,
  which resets the query's checkpoint again.
- The reverse asks for the nearest state YDB reaches in place, and the down
  file says what it keeps. A topic keeps the partitions the forward change
  added, and keeps auto-partitioning, paused, once the change turned it on.
  An async replication or a transfer keeps a credential the change added.
  A replication failed over before the forward change dropped it is not
  created again, since its tables stayed as ordinary tables. A reverse that
  leaves nothing YDB can run has no statement, only its recovery limit.
- What the forward change leaves of a replication or a transfer runs in the
  state the change found it in, so the reverse of a paused replication's
  connection change is an ALTER that YDB takes.
- A change on a target without the capability its forward change needed,
  such as `secrets` or `external_data_sources`, is not reversed: the reversal
  refuses it, as the host's embedded engine did.

The service also undoes a change of a table's own settings: a row table's
properties, such as its TTL and its partitioning, and a column table's
storage. What the forward change leaves is the description the table held
with the declared settings in place of its own; a setting the declaration
leaves unset stays as the table held it, except the TTL, which a table the
declaration gives none loses. A column's move to another family reverses to
a move back: what the forward change leaves is the column in the declared
family, with a default or storage setting the declaration does not state
kept as the column held it. A host that does not project a reverse's state
onto a column cannot plan that reverse and refuses it.

A change of a global
index's partitioning and read replicas reverses to every setting the index
held, as the host's embedded engine restores them (`restore every setting the
index held in place`), read replicas the forward change removed included: the
declaration keeps every setting it leaves out, so a reverse that stated only
some would keep the rest as the forward change left them. What the forward
change leaves is the index's own facet with the declared settings in place of
the ones it held. YDB removes no maximum partition count, so a maximum the
forward change gives an index that held none stays, and the down file says so
in the embedded engine's words.

The host plans such a reverse with the
declaration the forward change started from; the planner gives the index what
the reverse change asks for, against the facet the reverse predicts, rather
than what that declaration states. A host that does not list the feature gets
no reversal service and refuses every such change, naming the change kind
that has no reversal.

A topic reverses like any other object, and a migration file holds its
statements (see [Text units](connections-and-execution.md#text-units)).

## Common objects

The provider lists `schema.common-objects` (contract 25.3, ADR 0033 in Ptah),
and the ydb engine declares views, comments, roles and grants. A host that lists
the feature compares and orders these objects in its shared code, as it
does for every engine; the provider reads them, spells a declaration the way
YDB does, and writes each change as the host's embedded engine writes it. A
host that does not list it gets none of this: views stay native objects, and a
declared comment is refused as a table setting no creation states.

The comments of a table the host creates, of its columns and of its indexes,
and of a column or an index it adds to a table, reach the provider as comment
steps after the step that creates the object, as for any comment change. The
provider takes them off the declaration it creates the table from, and runs
the steps after its own steps on that table, with the exceptions below.

Views and comments use these shared stages:

| Step | Views | Comments |
| --- | --- | --- |
| Inspect | Each view, its query as the server stores it; on a dev realm without the realm's `PRAGMA TablePathPrefix`, which YDB stores at the head of a view the session created | The comment of each row table, of its columns and indexes, and of each view, from the user attributes YDB keeps them in |
| PrepareCommonObjects | The query in the server's form: its tokens joined by single spaces, comments and terminators left out | As declared |
| ValidateSchema | Refuses a view without a query, `WITH CHECK OPTION`, view attributes and materialized views | Refuses what an attribute cannot hold: a key over 100 bytes, a value over 4096 bytes, text that is not UTF-8, more than 10240 bytes on one table |
| Render | `CREATE VIEW ... WITH (security_invoker = TRUE) AS`, the query and `;` on lines of their own, then the view's comment; `DROP VIEW` | Ptah's `COMMENT ON TABLE`, `COLUMN`, `INDEX ... ON` and `VIEW`, `IS NULL` to remove one |
| PlanFeatures | Dropped before every table step and created after each one that does not only remove | After every step on the table that keeps it, except one that only removes; a comment on the table or a column also skips a step that changes only an index |

Under `schema.view-query-equivalence` (contract 25.33, ADR 0108 in Ptah) the
host asks the provider whether the query a declared view states and the query
the held view keeps are one query, and compares no YDB view query by its own
reading, which folds case. The provider reads both into the server's form and
compares them as they stand, as the host's embedded engine did: YDB names keep
case, so a query that changes only the case of a name, of a keyword or inside a
string is another query, and the plan replaces the view; a comment, a line
break or a terminator the server drops changes nothing.

The server's form of a query was measured on 25.1.4.7 and 26.2.1.14 alike: a
two- or three-character operator is one token except `>>`, which YDB writes
`> >`; `$x` is `$ x`; a number keeps its exponent and its suffix; a quoted
string or name, a multi-line string among them, is kept whole. A read of a view
applied once therefore equals the preparation of its declaration.

A step that only removes, such as a column's removal, is ordered after the
views and comments of the plan, not before them: the host places a removal
after its views and comments, and what a removal drops is no longer declared,
so no view the plan creates reads it and no comment the plan sets names it.
The one comment step that names a dropped column removes its comment: YDB
keeps the attribute once the column is gone and would give it to a column
added later under that name. The host sends that step for each dropped column
that held a comment (stokaro/ptah#5280), in its comment window, so it runs
before the removal.

A comment on a table or on one of its columns is not ordered after a step that
changes only an index of the table, such as an index's addition. The host
places the comment of a column it adds right after the column, before the
index additions, and the index step changes nothing such a comment names. A
comment on an index still follows the steps on its table.

YQL has no comment statement. `Execute` reads Ptah's `COMMENT ON` as the host's
YDB connection does and sets the one attribute it names through the table
service, after checking that the object is in the session's database or realm,
is a row table or a view of the kind the statement names, and for a non-empty
comment has the column or the index. A statement whose path holds nothing, or
one the session may not see, is refused before any write with a message that
names the path. A unit holding a comment statement beside
other statements, such as a commented view, runs as the queries a migration
file is split into, one statement after another. A comment statement in a
transaction, or one that cannot be read, refuses the batch before any unit
runs.

A comment is never a table property: the table facet leaves Ptah's
comment keys out, a declared attribute under one of them is refused, and the
new table of a rebuild takes the comments the read reported, which the host
carries on the observed table, its columns and its indexes of a planning
request.

Users and groups share YDB's principal namespace. Creation accepts lower-case
ASCII letters and digits. Groups have no login or password settings. Membership
can name a cluster-managed group, but cannot declare self-membership or make a
principal a member of a declared user. Inspection preserves every observed
membership, including edges whose principals are outside the managed role list.

A read leaves out of the description the users and groups the cluster made for
itself, the database's owner, and every user and group of a dev realm, and
names them as roles the server holds outside its scope. Under
`schema.role-scope-reasons` (contract 25.26, ADR 0076) the read also says why,
and the host's note gives the reason the host's embedded engine gave: `because
the cluster made them or they own the database, or the read was of a dev realm,
which shares its database's users and groups`.

A user's password is set when the user is created, from a YAML, Go or YQL
declaration, and is never read back: a declaration that names another password
plans nothing, as the host's embedded engine plans it. A YQL document's
password reaches the host under `source.sql-role-passwords` (see [SQL schema
reading](yql-reference.md#sql-schema-reading)).

The password never enters a plan. Under `schema.password-inputs` (contract
25.22, ADR 0071) the host sends a declared password, or a password hash, as an
execution input, and the statement names the input where the password stands:
``CREATE USER `app` PASSWORD $PTAH_SECRET_PASSWORD_APP;``. YDB refuses that
statement as written. `DescribeExecutionInputs` reports the input, and the
provider writes the bound value in as a string literal only when it runs the
statement, with `HASH` for a password hash, and keeps it out of any error the
server answers with. The host binds the declared password when it plans from
the schema and applies in one run, and the variable's value for a saved plan
or a migration file.

A host that does not negotiate the feature sends the
password itself, and the provider refuses the user rather than write the
password into the plan.

`schema.common-access` (contract 25.5) carries independently covered owners and
memberships. An owner's login state is optional: unknown remains unknown,
including when the server refuses its principal views. Principal queries read
names and login settings without reading passwords or hashes. The host refuses
security analysis with incomplete access knowledge. A host that does not
negotiate this feature receives uninspected role and grant coverage.

Permissions use canonical YDB names after preparation. Column grants,
`WITH GRANT OPTION`, `PUBLIC`, and permissions YDB does not recognize are
refused. Database grants name the opened database. On older release lines a
root-level grant also needs the captured absolute database path; rendering
refuses an unbound path. Native grant targets retain the provider's naming,
including literal dots in resource-pool names.

Inspection marks as implicit the entries YDB gives an object when it creates
it, which no GRANT made, as the host's embedded engine marks them: the entries
of the default resource pool that name `ydb.granular.select_row` and
`ydb.granular.describe_schema` together, and the `ydb.granular.describe_schema`
entry a new secret gets for every subject whose effective permissions on the
secret's directory carry that permission.

Mixed plans check captured native state before changing its access. A native
alteration precedes grants, revocations, and principal changes. A grant or a
revocation on a directory changes the directory's access list and occupies no
path, so the native planners leave it out of their path checks; a native
creation in that directory, or below it, comes first, because creating the
object creates a missing directory and YDB grants nothing on a path that holds
nothing. A final native
drop accounts for matching revocations by removing the object's access; a
temporary external-table drop keeps revocations after restoration. These plans
require the negotiated `planning.lifecycle` feature. Workload operations retain
their routing order after principal creation and before principal removal.

Standalone sequences are unsupported. A sequence declaration is refused before
rendering.

## Migration history

The history service owns its physical YDB storage. Native revision keys are signed
64-bit integers; Atlas revision keys retain exact strings, including an empty
identity. Each row contains the key and a deterministic binary SDK revision
record. The record retains nanosecond timestamps and durations, direction,
progress, diagnostics, checksums, Atlas type bits, and ordered partial hashes.

Creation installs the table layout and a `ptah.history` ownership attribute in
one native request. The marker is `stokaro/ptah-ydb-plugin/history-v1/native`
or `stokaro/ptah-ydb-plugin/history-v1/atlas`. This is a storage format version,
not a stable SDK release. Reads and writes verify the marker, column types,
primary key, and absence of automatic behavior such as TTL or changefeeds.
Unknown fields, malformed records, altered layouts, and unmarked tables are
refused, with one exception: the revision tables Ptah's built-in YDB engine
wrote.

That engine kept a database's migrations in tables of its own layout, with no
attribute: the native `schema_migrations` (version, description, applied_at,
state, applied, total, error, error_stmt, execution_time_ms, checksum), and
the Atlas-compatible `atlas_schema_revisions` that `ptah-compat migrate apply`
writes. A revision table with no attribute and exactly one of those layouts is
read as history, and every revision written to it keeps that layout, so a
dirty migration the engine left stays dirty with its progress, a resume
continues it, and the engine still reads the table: a down run is suffixed
`:down` in `state` as the engine wrote it, an empty error is NULL in the native
table and an empty string in the Atlas one, and no partial hashes are the JSON
`null`.

Those layouts keep the time to the microsecond, the native run time to
the millisecond, and no operator version or partial hashes in the native
table, so a revision written there holds no finer part, and a native record
with one is refused. Under the engine's default names such a table is also
left out of a schema read, as the engine left it out, so a plan for its
directory does not drop it. A table that is neither the provider's nor one of
those layouts is still refused. The provider's own Atlas format preserves SDK
revision semantics; it does not reproduce the Atlas SQL column layout or
promise interchange with external Atlas tools.

Mutations compare the complete expected record before inserting, replacing, or
deleting it. The comparison and write share one native serializable transaction.
A caller transaction binds history and application writes to the same commit
or rollback. Otherwise the provider opens and finishes a private transaction.
A result inside a caller transaction remains provisional until commit succeeds.
Conflicts cannot overwrite a committed winner; uncertain outcomes never authorize
an automatic retry. Request IDs are correlation identifiers.

Reads distinguish a missing store from an empty store. Namespace traversal is
refused. A failed native listing is an error rather than evidence of absence.
Snapshots are bounded by the caller's revision limit and a 3 MiB size limit;
exceeding either returns no partial history. An unreadable history snapshot or
failed dispatched history mutation makes its caller transaction rollback-only.
History storage alone does not establish the whole migration workflow.
[Host-driven tests](../CONTRIBUTING.md#host-driven-integration-checks) separately exercise planning, application, and migration status.

## Migration journals and tags

The metadata service stores exact SDK records. A journal key combines the run ID
and signed sequence number; duplicate identities are refused even when the
payload is identical. A tag key preserves its exact name. Versions remain signed
64-bit values, including zero, and timestamps retain nanoseconds. The host owns
event meaning, attempt pairing, tag normalization, and migration recovery.

Creation installs the native layout and the `ptah.metadata` ownership attribute
in one request. Its value is `stokaro/ptah-ydb-plugin/metadata-v1/log` for journals
or `stokaro/ptah-ydb-plugin/metadata-v1/tags` for tags. Preparation and operations
refuse unmarked tables, unknown markers, altered columns or keys, indexes,
changefeeds, and automatic deletion settings. Reads also refuse unknown,
noncanonical, corrupt, or mismatched record identities.

The log and tag tables
Ptah's built-in YDB engine wrote, `schema_migrations_log` (run_id, seq,
operation, version, state, actor, actor_source, checksum, logged_at, error)
and `ptah_migration_tags` (tag, version, recorded_at), are read and written in
that layout, with no attribute, as its revision tables are; the time is kept to
the microsecond, and an empty actor, checksum or error is NULL. Under those
default names they are left out of a schema read too.

Log writes use a separate serializable transaction and remain durable when an
execution transaction rolls back. Log scopes cannot name that execution
transaction. Tag mutations use the selected execution handle, or a private
serializable transaction when no handle is active. Preparation inside a
transaction only validates an existing store; it never creates schema. Tag
replacement and deletion validate any existing record before changing it.
Deletion reports existence from an atomic read, not from affected-row counts.
A missing tag is distinct from a tag at version zero.

Each dispatched mutation runs once. A lost write or commit acknowledgment is
UNKNOWN, without automatic replay. A storage read or mutation error on the
selected SQL transaction makes it rollback-only. Lock loss refuses further
journal and tag writes, including durable log writes outside an execution
transaction.

Opening a reader captures a complete immutable snapshot in one serializable
transaction. A count and ordered stream must agree before any page is exposed.
The snapshot uses a private temporary file, so memory is bounded by a record
and an RPC page rather than journal length. Unix unlinks the file immediately;
Windows uses native delete-on-close. Closing the handle or terminating the
provider releases the file on both platforms.

Pages contain at most 256 records and 3 MiB including their envelope. Oversized
records and incomplete reads are refused, never truncated. Snapshot handles
are single-use and bind the exact session, transaction, kind, and store. Page
indices advance once; a failed or uncertain page must be closed. Closing an
unused ID fences a late open. Each session retains at most 1,024 reader IDs and
allows at most 16 unfinished snapshots. Completion and close release snapshot
files; transaction finish and session close release their remaining readers.

The live tests cover journal durability, tag commit and rollback, same-transaction
snapshot immutability, journals larger than one RPC, storage reservations,
corruption, and reader fencing. Metadata unit tests also run on Windows to
exercise native file cleanup.

## Coordination locks

The lock service uses exclusive ephemeral semaphores on the database-root
`ptah_locks` coordination node. Each acquisition has its own native session.
The provider creates a missing node with strict read and attach consistency.
Ptah's built-in YDB engine creates the node with neither setting, which YDB
reads as relaxed reads, so every database that engine locked holds such a
node: the provider alters a node that sets neither mode to strict reads and
attachment before it takes a lock there, as a run of that engine took its
locks there, and a session of that engine that still holds a lock keeps it. A
user who may use the node but not alter it is refused with the right to grant.

Any other node must have both settings explicitly strict; the provider refuses
one someone set otherwise without changing it. The refusal is a
`NOT_ACQUIRED` receipt that holds nothing, and its reason names the node and its
two settings.

YDB's CLI (2.25.0 and 2.29.0, bundled with 25.1.4.7 and 26.2.1.14) and YQL have
no command for a coordination node, so making such a node strict goes through
the coordination service. With
[ydb-go-sdk](https://github.com/ydb-platform/ydb-go-sdk), alter the node in
place:

```go
err := driver.Coordination().AlterNode(ctx, path.Join(driver.Name(), "ptah_locks"), coordination.NodeConfig{
	ReadConsistencyMode:   coordination.ConsistencyModeStrict,
	AttachConsistencyMode: coordination.ConsistencyModeStrict,
})
```

`DropNode` on the same path removes the node instead, and the next lock creates
it strict. Remove it only while no run holds a lock there: dropping the node ends
every lock session on it. YDB's default read mode can
return stale state during leader changes; see the
[YDB coordination documentation](https://ydb.tech/docs/en/reference/ydb-sdk/coordination).

The provider finds the node by describing its path rather than by listing the
database root, so a user granted the node takes the lock without the right to
list the root. A server that refuses the user the lock is answered with a
`NOT_ACQUIRED` receipt, and its reason names the step and the right: the node is
missing and the user may not create it, the user may not use the node, or the
server refused the session on the node. The first two are the host's embedded
engine's words. For the third that engine reports a timeout, because its SDK
retries a refused session without saying why, while the server answers at once
with UNAUTHORIZED.

The receipt confirms that nothing was acquired, so the host
neither fences the session nor sends a cleanup release, and the session takes
the lock once the user is granted the node. A host that lists `errors.reasons`
prints the reason after the outcome; see [Error reasons](catalog-and-errors.md#error-reasons).

A `TryLock` that takes no lock says why in its receipt: another session holds
the lock, the session has an active transaction, it lost a lock guarantee, it
already holds 16 locks, the server refused this user the lock, or the node is
not strict.

Acquisition is nonblocking. Only a confirmed BUSY permits the host to try a new
handle under its wait policy. Handles are single-use and bound to the exact
session and name. Release of an unused handle fences a late acquisition. A
session retains at most 1,024 handles and holds at most 16 active locks.
Acquisition during an execution transaction is refused. Read-only lock previews
are not represented by this SDK revision.

A native bidirectional stream sends each acquisition once and checks native
operation status and request identity. It does not reconnect or reacquire after
a transport failure. Keepalives run every two seconds; the native session timeout
is ten seconds. Ownership checks run every second and verify the strict node
configuration, exact session owner, and exclusive ephemeral semaphore count.
A confirmation expires after five seconds. Both monotonic and wall-clock elapsed
time are checked, including before refreshing a confirmation after a pause.

Every SQL unit, native coordination operation, history or metadata mutation, and
transaction commit verifies guarding locks. Storage creation checks the lease
immediately before native dispatch too.
Loss or uncertainty cancels guarded calls and permanently fences further writes
in that provider session. Rollback, release, and close remain available. A new
owner does not repair the old session. Failure after dispatch may have effects;
an acknowledgment racing ownership loss is reported as uncertain.

Release requires successful semaphore-release and session-stop acknowledgments.
Local stream shutdown is insufficient. Cleanup continues with its own timeout
after caller cancellation and retains uncertainty on repetition. Provider death
leaves the server to expire the native lease. The coordination node remains for
later runs; its ephemeral semaphores disappear after their owners leave.

These are advisory leases, not transactional fencing tokens for SQL or DDL.
Fresh checks and cancellation cannot undo a statement already executed or make
nontransactional DDL atomic with ownership. Live tests cover competing provider
processes, independent names, immediate takeover after confirmed close, process
exit, node removal, and refusal of execution/history/commit after loss. Controlled
clock tests cover stale confirmations without relying on a scheduled watcher.
