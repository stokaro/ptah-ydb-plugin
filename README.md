# Ptah YDB provider

This repository owns the executable YDB provider for [Ptah #4204](https://github.com/stokaro/ptah/issues/4204).
It is under development. Inspection covers row tables and their indexes and
changefeeds, topics, views, coordination nodes, external data sources, and
external tables. Native SQL execution and session-bound data transactions are
implemented, together with provider-owned migration history, event journals,
tags, and coordination locks. Offline YAML decoding is available for coordination-node declarations.
Offline comparison, planning, and rendering produce native coordination
operations. Execution verifies their captured preconditions and reads back the result.

The executable opens native YDB connections and reads row-table schemas
through the public Ptah SDK. It supports anonymous, URL user/password, and URL token authentication.
Session receipts contain the selected database and measured server version.
The implementation does not import the Ptah application module.

The provider ID is `github.com/stokaro/ptah-ydb-plugin`. Model kinds use the `github.com/stokaro/ptah-ydb-plugin/` namespace. This changes
the experimental model identities; pre-v1, no compatibility with the earlier
unpublished names is owed. Older envelopes and plans are refused.

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
atomic revision comparisons, shared data/history/tag transactions, durable event
journals, immutable paged metadata snapshots, process lock
exclusion, confirmed release, and write refusal after lease loss,
native row-table observations, offline
normalization, index implementation tables, changefeed consumers and retention,
standalone native objects, exact-directory completeness, and refusals for
incomplete reads. Coordination tests cover source decoding through planning,
rendering, native execution, and inspection. They do not establish complete
migration support or host CLI integration.

## Native CLI development acceptance

Development checks with the experimental host adapter verified reviewed SQL
migration execution and rollback against YDB 25.1.4.7 and 26.2.1.14. The checks
also exercised status, journals, tags, and default locking through the executable.
Schema changes used `--tx-mode none` for `migrations up` and a
`-- +ptah no_transaction` directive in the down file. YDB does not provide
transactional schema changes.

The host and provider were built with a temporary SDK revision 21 workspace.
These checks do not establish installation from the committed dependency pin.
Host `schema inspect --format json` still refuses observed row-table facets
without desired model conversion, including column properties. The native
inspection service retains those observations; export and table comparison
require further model support.

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
SQL units use a pinned connection; native coordination units use their native
service. Dispatched writes are not retried.
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

Coordination operations use native envelopes. Other operation models, common
SQL rendering and analysis remain to be extracted. The host's
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
noncanonical, corrupt, or mismatched record identities. Existing Ptah SQL stores
are not adopted; pre-v1, so no compatibility is owed.

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

## Operational storage reservations

Inspection recognizes history and metadata storage from exact ownership attributes and
complete native layout, before extracting indexes or changefeeds. A marked
table with an altered layout refuses the entire inspection. An unknown marker
version is also refused. An unmarked table remains managed even when its name
or layout matches a history table.

Confirmed history, journal, and tag stores are returned as SDK reservations
instead of managed tables. A reservation occupies the name for every supported native scheme-object
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

Operational storage reservations support host collision and reference guards. They do not
establish native directory-cascade safety; containment qualification remains
pending. Coordination planning refuses overlapping directory lifecycles. Markers
are storage-format identifiers, not an access-control mechanism against a database administrator.

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
Column and key arrays retain their observed order. Desired coordination-node declarations are available as described below.
Other desired models, their comparisons, and planning remain pending.

Views retain the query returned by the view service and the attributes read
through the table service on the same path. This includes comments and custom
attributes. Coordination nodes retain native consistency modes, timing, and
counter configuration; the envelope owns the node path. External data sources
retain source type, location, authentication references, and native properties.
External tables retain source references, location, ordered typed columns, and
content options without rewriting their native keys or JSON values.

## Offline coordination declarations

The source service accepts the captured YAML document key `coordination_nodes`.
The host parses YAML and supplies typed values and original byte spans; the
provider interprets this grammar without reading files or opening a session.

```yaml
coordination_nodes:
  workers:
    schema: app
    self_check_period: PT2S
    session_grace_period: PT5S
    read_consistency_mode: strict
    attach_consistency_mode: relaxed
    rate_limiter_counters_mode: detailed
```

Each entry is a mapping. Its key supplies the node name unless `name` overrides
it. `schema` is an exact directory relative to the database; `struct_name`
retains source provenance. These fields and every setting require text.
Unknown keys, explicit nulls, numeric coercion, repeated fields, duplicate
object identities, and collisions with declared tables in the same captured
namespace refuse the whole source batch. Root `ptah_locks` is reserved for
runtime use; the same leaf name in a user directory is allowed. Inspection
still returns root coordination nodes as ordinary observations.

Periods use positive ISO8601 durations and must preserve whole milliseconds.
The self-check interval must be 500–10,000 milliseconds. The grace interval
must be at least the effective self-check interval plus 1,000 milliseconds and
at most 30,000 milliseconds. The provider refuses configurations the server
would clamp. Mode strings are case-insensitive in source and stored lowercase.

Omitted settings remain absent in the desired model. They request server
defaults when the node is created or compared: a 1,000-millisecond self-check,
10,000-millisecond grace interval, relaxed reads, strict attachment, and
aggregated counters. Explicit settings equal to defaults remain explicit.
Offline normalization does not assert desired/observed equivalence.

The decoder returns native objects with source traces and coverage limited to
the models enrolled by each document. An empty mapping declares no nodes within
that scope; an empty model scope stays empty. No successful prefix survives an
invalid entry or cancellation. Desired and observed models have distinct
encodings and definition digests; observation coverage contains only observed
models.

The subprocess tests exercise decoding and normalization without a database
address. HCL, Go annotation and SQL adapters and desired/observed conversion
remain pending.
This service alone does not make the YAML example executable as a migration.

## Coordination comparison

The offline `models.compare` service compares desired coordination declarations
with captured native observations. Its context must establish the
`coordination_nodes` capability, which session receipts advertise for the native
implementation. Omitted settings compare using server defaults;
source holder names do not change native configuration. Removing an explicit
nondefault setting produces a change back to the default. Raw stored values that
the server clamps refuse comparison.

The `github.com/stokaro/ptah-ydb-plugin/coordination-node-change` model carries complete `before` and `after`
operands. The native `before` retains access metadata. A null operand means
established absence. Normalization reconstructs impact, removal, and access
facts from these operands. Creation has unknown inherited access policy;
alteration preserves access; dropping a node removes its runtime state and access.
Planning lowers these changes to the `github.com/stokaro/ptah-ydb-plugin/coordination-node-operation` model,
which rendering preserves as a native execution envelope.

An unread object does not establish absence. Independent coverage sources must
all establish completeness; overlapping unknown regions override complete
regions, and explicit subject claims override namespace claims. Namespace
components, target scopes, and exact empty paths retain their boundaries.
An incomplete desired source preserves known current configuration. Missing
namespace coverage also produces an undecided diagnostic.

The comparison refuses contradictory identities, forged normalization receipts,
and collisions with declared relations. Database defaults require an explicit
host binding. Required nodes are not removed. Exclusions for this model match
exact relative paths such as `app/workers`, without case folding or trimming.
The database-root `ptah_locks` observation remains visible and produces an
undecided lifecycle diagnostic; its name does not prove provider ownership.

Offline subprocess tests cover decode, compare, and change normalization without
opening a session. Live tests compare native observations after closing their
session on both qualified YDB lines.

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

## Native coordination planning and execution

The offline planner preserves complete before and after operands. Each step
has an object effect, a shared `ydb.scheme-path` effect, and a forbidden SQL
transaction requirement. Table-to-node and node-to-table replacements require
an explicit common path handoff. Unsupported common footprints and overlapping
parent/descendant paths are refused until a directory lifecycle is captured.
The parent directory must already exist; execution never creates or deletes it.

Rendering validates the complete operation receipt and emits a native envelope.
It does not generate coordination SQL. Execution uses native CreateNode,
AlterNode, and DropNode requests. A nonempty database identity must equal the
selected session database. The provider validates every envelope and transaction
constraint before executing any unit in the batch.

Before each native write, execution lists the exact parent directory and reads
the node. The current configuration and access metadata must match the captured
observation, or creation must have established absence. A stale operation is
refused before dispatch. Effective settings are sent explicitly, including
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
batch refusal, and lease loss on both tested YDB release lines. Host CLI routing,
other object lifecycles, and complete migration planning remain pending.

## Offline SQL analysis

The SQL analysis service reads default-mode YQL without a database session. It
preserves source byte ranges and line numbers, original tokens, and quoted case.
Semicolons inside quoted or multiline literals, comments, action definitions,
inline actions, and lambda bodies remain inside their enclosing statement.
Leading and trailing comments stay in the returned source ranges.

The scanner and value decoder were extracted from Ptah's YQL implementation,
with its lexical regression corpus. The provider keeps no host-package import.
Live tests compare reconstructed statements with results from both supported
YDB test lines, including escaped quotes, multiline strings, compound bodies,
and numeric suffixes.

Native safety judgments cover topic and consumer positions, transfer consumers,
replica tables, coordination state, secrets, external data access, and streaming
checkpoints. Common table creation and a single ADD COLUMN clause retain the
host's common DDL assessment. Unmodeled effects, including arbitrary compound
bodies and changes to automatic expiration, report UNKNOWN and require review.
The analysis does not replace the server's full SQL grammar validation.

Malformed literals, incomplete compound bodies, invalid UTF-8, NUL, and unsupported
translation settings refuse the whole batch. Only the default lexer and
`--!syntax_v1` are accepted; ANSI mode is refused even when its directive appears
later in a source, because splitting could move it to a new query's head.
Whitespace-only sources are accepted. A source containing only comments or
terminators is refused because SDK revision 21 requires tokens for every
non-whitespace source range. No prefix of a refused batch is returned.

A request contains at most 256 sources and 3 MiB. Each source contains at most
65,536 significant tokens, and the complete response is bounded by 3 MiB.
Cancellation returns no analysis. The native CLI still requires integration of
YDB's table comparison rules; the provider does not claim the common table
identity contract to bypass that requirement.

## Extraction still required

The source reference is Ptah commit `7b4b19f49fadf03ce81def2f6168ea60b3261a71`.
Remaining work includes the other desired schema models and source formats,
comparison, planning, rendering, and operation models for other objects,
capability detection, environment credential sources,
monitoring, and dev-database provisioning. The provider advertises implemented
inspection, normalization, coordination YAML source decoding, comparison, planning,
rendering, SQL and native coordination execution, transaction, history, metadata,
lock, and offline SQL analysis services. The remaining capabilities stay
unadvertised.

The host's embedded implementation still needs removal when these operations
are wired through this executable. Provider release workflows, default Ptah bundling,
and independent installation acceptance remain pending. No v1 stability or
full YDB support is claimed by this development state.
