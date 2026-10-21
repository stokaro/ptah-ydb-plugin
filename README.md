# Ptah YDB provider

This repository owns the executable YDB provider for [Ptah #4204](https://github.com/stokaro/ptah/issues/4204).
It is under development. Inspection covers row tables and their indexes and
changefeeds, topics, views, coordination nodes, external data sources, and
external tables. Native SQL execution and session-bound data transactions are
implemented, together with provider-owned migration history and coordination
locks. Schema planning
is still pending.

The executable opens native YDB connections and reads row-table schemas
through the public Ptah SDK. It supports anonymous, URL user/password, and URL token authentication.
Session receipts contain the selected database and measured server version.
The implementation does not import the Ptah application module.

## Build and test

The current source uses experimental SDK revision 21 reservations. That revision
is not published yet; `go.mod` still pins published revision 19. A standalone
build from that pin is incomplete. Development checks currently use a temporary
workspace containing this module and Ptah's `provider-sdk` checkout. The
workspace is not committed and is not independent installation evidence.

After publishing revision 21 and updating the dependency pin, run:

```sh
GOWORK=off go test -race ./...
GOWORK=off CGO_ENABLED=0 go build -o bin/ptah-plugin-ydb ./cmd/ptah-plugin-ydb
```

The executable requires the SDK's authenticated local subprocess bootstrap.
Run it through a Ptah SDK client rather than invoking it as a command-line tool.
The committed module has no `replace` directive or workspace file. The SDK
pin update remains required before the standalone commands and CI can pass.

Live checks require an isolated test database. Schema tests create uniquely
named directories, tables, and topics and remove them afterward. Session tests only
read server metadata.

```sh
PTAH_YDB_TEST_URL='ydb://localhost:2136/local?go_balancer=disable' \
  GOWORK=off go test -race -tags integration ./integration -count=1 -v
```

The test servers enable `enable_vector_index` and
`enable_external_data_sources`; the 26.2 fixture also enables
`enable_add_unique_index`. Fulltext creation is expected to be refused on 25.1.

CI runs this process test against YDB 25.1.4.7 and 26.2.1.14. It also rejects
host-module imports and local module replacements. These checks cover sessions,
SQL execution, commit/rollback, range conflicts, durable migration history,
atomic revision comparisons, shared data/history transactions, process lock
exclusion, confirmed release, and write refusal after lease loss,
native row-table observations, offline
normalization, index implementation tables, changefeed consumers and retention,
standalone native objects, exact-directory completeness, and refusals for
incomplete reads. They do not establish
planning or complete migration support.

## Connection addresses

Use `ydb://host:2136/database` or `ydbs://host:2135/database` for TLS.
`database`, `token`, and `go_balancer` are the supported query parameters.
A token and URL user/password cannot be combined. Unknown or repeated
parameters, conflicting database paths, and malformed values are refused.
Credentials travel in the session-open request and are omitted from receipts,
SDK addresses, and error diagnostics. Native errors currently report only the
failed operation, which limits diagnosis until structured redaction is added.

Session close releases both the SQL pool and its parent native driver, even
when the RPC context is canceled. Cleanup results are retained for repeated
closes. A process accepts at most 1,024 lifetime session handles; start a new
provider process when that bound is reached.

## Execution and transactions

Execution sends native SQL units through YDB's query service. A batch is checked
before its first unit runs. Unsupported native envelopes refuse the whole batch.
Each request uses a pinned connection; dispatched writes are not retried.
Request IDs correlate calls and do not provide exactly-once execution.

Results distinguish a confirmed prefix from an uncertain remainder. A failed
dispatched unit reports UNKNOWN, even when earlier units succeeded. A unit may
contain several SQL statements, so its error cannot prove that none ran.
Cancellation between units reports the confirmed prefix. Diagnostics omit raw
driver errors and SQL text.

Transaction handles belong to one provider session. DEFAULT and SERIALIZABLE
both use YDB's native serializable read/write mode; receipts retain the
requested isolation. Begin cancellation remains connected while the database
transaction opens, then detaches when the handle is returned. Ending the begin
RPC therefore does not roll back later work.

A session permits one active transaction. Execution must name its handle while
it is active. Commit and rollback retain their terminal result, including
UNKNOWN, without issuing a second database operation. Rollback of an unused
handle fences a late begin. The SDK client sends rollback after a failed begin,
including a duplicate begin; callers must not reuse handles.
Each session retains at most 1,024 transaction handles.

Closing a session rolls back unfinished work even if the close caller was
canceled. Connection acquisition for begin and native transaction calls have a
30-second bound. Unconfirmed cleanup remains an error. YDB schema changes are not transactional; session
receipts explicitly report `transactional_ddl=false`. Live tests verify that
DDL and SQL transaction-control statements cannot escape an explicit data
transaction. See [YDB transaction semantics](https://ydb.tech/docs/en/concepts/transactions).

Native envelopes for provider-specific operations, SQL analysis, rendering,
logs and tags remain to be extracted. The host's
extended comment and coordination SQL forms are not implemented here.

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
refused. Older Ptah tables are left untouched. This changes storage behavior;
pre-v1, so no compatibility is owed. The Atlas format preserves SDK revision
semantics; it does not reproduce the legacy Atlas SQL column layout or promise
interchange with external Atlas tools.

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
History storage alone does not establish complete migration support. Planning
and complete host integration remain pending.

## Coordination locks

The lock service uses exclusive ephemeral semaphores on the database-root
`ptah_locks` coordination node. Each acquisition has its own native session.
The provider creates a missing node with strict read and attach consistency.
An existing node must have both settings explicitly strict; the provider refuses
an incompatible configuration without changing it. YDB's default read mode can
return stale state during leader changes; see the
[YDB coordination documentation](https://ydb.tech/docs/en/reference/ydb-sdk/coordination).
This changes the requirements for an older Ptah lock node; pre-v1, so no
compatibility is owed. Configure that reserved node explicitly before using it.

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

Every SQL unit, history mutation, and transaction commit verifies guarding locks.
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

## Operational storage reservations

Inspection recognizes history storage from its exact ownership attribute and
complete native layout, before extracting indexes or changefeeds. A marked
table with an altered layout refuses the entire inspection. An unknown marker
version is also refused. An unmarked table remains managed even when its name
or layout matches a history table.

Confirmed history stores are returned as SDK reservations instead of managed
tables. A reservation occupies the name for every supported native scheme-object
kind and for common tables. Exact UNREPRESENTABLE subject coverage overrides
directory completeness at these occupied names, so omission does not assert
absence. Filtered reads return reservations only from the exact directories
they inspect. Names or layout alone never establish ownership.

Coordination nodes remain managed observations. YDB returns SUCCESS for repeated
native CreateNode requests, so a successful create cannot prove that this
provider created the node. Installing an ownership marker based on that result
could claim a concurrently created foreign node. Lock-node ownership and
reservation need a separate initialization contract and remain pending. No
node is hidden or reserved merely because it is named `ptah_locks` or has
strict consistency settings.

History reservations support host collision and reference guards. They do not
establish native directory-cascade safety; native planning and containment
qualification remain pending. Markers are storage-format identifiers, not an
access-control mechanism against a database administrator.

## Schema observations

Inspection reads the native scheme, table, topic, view, and coordination APIs. Common columns preserve
YDB type spelling, Decimal precision/scale, nullability, and key membership.
Provider-owned table properties retain key order, TTL, column families,
partitioning, storage settings, attributes, and access metadata. Column
properties retain family assignment and typed literal or sequence defaults.
The observations carry their complete native field definitions and digests.
Normalization works offline after the database session closes.

Schema selectors name exact directories relative to the selected database.
Use `/` for its root. An unfiltered read walks subdirectories. System
(dot-prefixed) directories are excluded; user tables are not excluded merely
because their names begin with a dot. Traversal and snapshot size are bounded.
Coverage names inspected subjects and records native-object completeness for
each directory whose listing and child reads finished. These regions bind the
exact database, empty catalog, and exact schema directory. An empty directory
therefore establishes native-object absence there. Unread descendants, excluded
system directories, and other namespaces remain unknown. Table and column
facets keep subject-only coverage.

Unknown native fields and enum values are refused before JSON conversion.
Shared topic consumers, column tables, unextracted native object kinds, and
replication-owned tables refuse inspection. A failed traversal returns no snapshot.
These limitations preserve data for the remaining extraction work.

Observed property models use YDB's typed protocol definitions inside this
provider. The neutral SDK carries their envelopes. Common fields and runtime
statistics are excluded from property payloads; they cannot become a second
writable representation. Index observations preserve sync, async, unique, vector, prefixed vector,
and fulltext variants. The provider reads every kind-specific implementation
table, including its columns, keys, and partition settings; parent table
metadata alone does not expose those settings. Missing implementation tables
or non-ready indexes refuse inspection. An empty index facet records the
absence of indexes on that inspected table.

Changefeed observations combine the table API description with a topic API
read for each child stream. They retain modes, formats, state, retention,
consumers, access metadata, and attributes, including replication bindings.
Initial scan presence is retained after completion; progress counters are
excluded. An empty changefeed facet records absence on its inspected table.
Standalone topics use the same reader and normalization rules, with their own
object identities.

Topic observations preserve partition settings and quotas, retention, codecs,
consumer options, and attributes. Live partition topology and topic/consumer
statistics are excluded. A missing child description refuses the entire read.

Indexes, implementation tables, changefeeds, and consumers are sorted by name.
Column and key arrays retain their observed order. Desired models, semantic
comparisons, and planning are still pending.

Views retain the query returned by the view service and the attributes read
through the table service on the same path. This includes comments and custom
attributes. Coordination nodes retain native consistency modes, timing, and
counter configuration; the envelope owns the node path. External data sources
retain source type, location, authentication references, and native properties.
External tables retain source references, location, ordered typed columns, and
content options without rewriting their native keys or JSON values.

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

Fixture replay covers prior YDB 25.1 and 26.2 captures. A subprocess test checks
SDK normalization without opening a database session. Live column-table
inspection still refuses: monitoring transport and the combined live reader
are not connected yet. These tests do not establish live column-table support.

## Native protocol generation

The upstream Go bindings lack some native index, sequence, changefeed, and consumer fields. The
provider's typed definitions preserve their native wire numbers; attribution
and the pinned upstream source are in `third_party/ydb/NOTICE.md`. Unsupported
fields remain errors. Runtime statistics are omitted from schema observations.

With protoc 35.1 on `PATH`, regenerate with:

```sh
sh scripts/generate-native-schema.sh
```

The Go generator is pinned in `go.mod`. CI regenerates these definitions and
requires unchanged generated files.

## Extraction still required

The source reference is Ptah commit `7b4b19f49fadf03ce81def2f6168ea60b3261a71`.
Remaining work includes desired schema models, offline source formats,
comparison, planning, rendering, native operation envelopes, migration
logs/tags, capability detection, environment credential sources,
monitoring, and dev-database provisioning. The provider advertises implemented
inspection, normalization, SQL execution, transaction, history, and lock services. The
remaining capabilities stay unadvertised.

The host's embedded implementation still needs removal when these operations
are wired through this executable. Provider release workflows, default Ptah bundling,
and independent installation acceptance remain pending. No v1 stability or
full YDB support is claimed by this development state.
