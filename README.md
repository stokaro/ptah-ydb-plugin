# Ptah YDB provider

This repository owns the executable YDB provider for [Ptah #4204](https://github.com/stokaro/ptah/issues/4204).
It is under development. Inspection covers row tables and their indexes and
changefeeds, topics, views, coordination nodes, external data sources, and
external tables. Native SQL execution and session-bound data transactions are
implemented, together with provider-owned migration history, event journals,
tags, and coordination locks. Offline YAML decoding is available for coordination-node declarations.
Offline comparison, planning, and rendering produce native coordination
operations. Native row-table creation and column family alteration are available through captured SDK planning requests.
Execution verifies captured preconditions and reads back the result.

The executable opens native YDB connections and reads row-table schemas
through the public Ptah SDK. It supports anonymous, URL user/password, and URL token authentication.
Session receipts contain the selected database and measured server version.
The implementation does not import the Ptah application module.

The provider ID is `github.com/stokaro/ptah-ydb-plugin`. Model kinds use the `github.com/stokaro/ptah-ydb-plugin/` namespace. This changes
the experimental model identities; pre-v1, no compatibility with the earlier
unpublished names is owed. Older envelopes and plans are refused.

## Releases

A tag that starts with `v` runs `.github/workflows/release.yml`, which
publishes a GitHub release through GoReleaser (`.goreleaser.yaml`). Each
release carries one archive per platform and a `checksums.txt` file:

```text
ptah-plugin-ydb_<version>_<os>_<arch>.tar.gz   (.zip on Windows)
checksums.txt
```

`<version>` is the tag without the leading `v`. The platforms are linux,
darwin and windows on amd64 and arm64. Every archive holds the
`ptah-plugin-ydb` binary (`ptah-plugin-ydb.exe` on Windows), `README.md` and
`LICENSE`. The binaries are built with `CGO_ENABLED=0` and `-trimpath`.

`checksums.txt` lists the SHA-256 digest of every archive in `sha256sum`
format (`<hex digest>  <file name>`). A host pins a release by version and by
the digest listed there for its platform's archive.

Pull requests that touch the release inputs run `goreleaser check` and a
snapshot build, then verify the snapshot's `checksums.txt`. To try it locally:

```sh
GOWORK=off goreleaser release --snapshot --clean
(cd dist && shasum -a 256 -c checksums.txt)
```

## Build and test

The module pins published experimental SDK revision 23, including operational
storage reservations and offline table preparation. Builds require neither the Ptah application source nor a
local SDK checkout.

Run the unit suite and build the executable:

```sh
GOWORK=off go test -race ./...
GOWORK=off CGO_ENABLED=0 go build -o bin/ptah-plugin-ydb ./cmd/ptah-plugin-ydb
```

The executable requires the SDK's authenticated local subprocess bootstrap.
Run it through a Ptah SDK client rather than invoking it as a command-line tool.
The committed module has no `replace` directive or workspace file.
`scripts/check-module-boundaries.sh` rejects local replacements and host-module
dependencies.

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

The host and provider were built against the published SDK revision 21 pin
with `GOWORK=off`. These are source-build checks; packaged installation and
default bundling remain separate requirements.
Host `schema inspect --format json` exports the native desired model, including
provider-owned values and coverage. Common collections use names such as
`Tables`, `Fields`, and `Indexes`. This JSON is diagnostic output, not a schema
file input. Table comparison and full native CLI planning remain pending.

## Connection addresses

Use `ydb://host:2136/database` or `ydbs://host:2135/database` for TLS.
`database`, `token`, `go_balancer`, and `use_env_credentials` are the supported
query parameters. A token, URL user/password and `use_env_credentials` cannot be
combined. `use_env_credentials` reads the `YDB_*` variables of the provider
process and fails when none is set, so an environment that would connect
anonymously is refused. An address with no credential source connects
anonymously. Unknown or repeated
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
SQL units use a pinned connection; native coordination, table creation and table alteration units
use their native services. Dispatched writes are not retried.
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

Coordination, row-table creation and column family alteration operations use native envelopes. Other
operation models and common SQL rendering remain to be extracted. The host's
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
or non-ready indexes refuse inspection. Internal sequence configuration is retained;
its mutable position is excluded after unknown-field validation. An empty index facet records the
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
Every observed model has a separate desired contract and an offline conversion.
Comparison and planning remain limited to coordination nodes; source adapters
for the other families remain pending.

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

The subprocess tests exercise decoding, normalization, and representation
conversion without a database address. HCL, Go annotation, and SQL adapters
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

Coordination exports retain owner, direct grants, and effective-permission
requirements in an optional `access` field. Its JSON field names use snake case.
Omission leaves access unmanaged. The YAML source grammar above does not accept
access declarations. Comparison reports unsatisfied access requirements as
undecided; change/operation normalization refuses them. Existing nodes can
change configuration while satisfying unchanged access requirements. Creation
with explicit access requirements is refused because the coordination service
cannot promise inherited permissions or apply grants.

The comparison refuses contradictory identities, forged normalization receipts,
and collisions with declared relations. Database defaults require an explicit
host binding. Required nodes are not removed. Exclusions for this model match
exact relative paths such as `app/workers`, without case folding or trimming.
The database-root `ptah_locks` observation remains visible and produces an
undecided lifecycle diagnostic; its name does not prove provider ownership.
Offline conversion and host schema export retain this observation, including
its configuration and access metadata. Export does not authorize management:
YAML declarations and change normalization still reject this root path.
This changes the desired model definition; pre-v1, no compatibility with the
earlier experimental definition is owed.

Offline subprocess tests cover decode, compare, and change normalization without
opening a session. Live tests compare native observations after closing their
session on both qualified YDB lines.

## Offline column declarations

The `values.convert` service converts column properties between distinct typed
observed and desired models. It preserves family assignment, typed literal
defaults, sequence configuration, compression settings, and encoding order.
Identity, storage type, and nullability remain in the common column contract.
An omitted default requests the native no-default setting and stays omitted
during normalization; `dropDefault: true` expresses removal explicitly.
Unknown fields, unknown enums, duplicate keys, null settings, and overlapping
defaults are refused. Explicit sequence configuration retains optional values,
including `cycle: false`.

Sequence positions are mutable data. An observation containing `setVal` refuses
conversion rather than exporting a request to reset the sequence. Conversion
validates the source payload and its captured normalization, retains exact owner
identity and ordered records, and returns no prefix on failure or cancellation.
It opens no connection and establishes no inspection coverage.

This supports column-property transport and JSON schema export. Column
comparison and planning are not advertised by this implementation.

## Offline row-table declarations

The row-table model preserves ordered primary keys, TTL, storage pools, column
families, attributes, partitioning policy, bloom-filter settings, read replicas,
tiering, and temporary lifetime. Unknown settings, duplicate keys or families,
invalid TTL modes, and null settings are refused. Attribute values remain exact,
including empty strings. The row-table reader interprets an unspecified native
storage kind as row storage; column storage retains its separate model.

Access declarations keep the owner and direct grants separate from requirements
on effective permissions. Effective permissions include inherited access under
[YDB's permission model](https://ydb.tech/docs/en/reference/ydb-cli/commands/scheme-permissions).
A present access declaration specifies the complete direct-grant set and the
required effective-permission set. These requirements do not add direct grants. Conversion
neither infers an inheritance switch nor changes permissions. The provider does
not yet advertise table or access comparison/planning.

Physical shard boundaries are runtime topology. A captured value containing them
refuses conversion rather than turning the current split points into creation
settings. Partitioning policy remains declarative. Statistics and transient
object identity cannot enter the property model. Conversion preserves a complete
mixed table/column batch, works after the session closes, and establishes no
inspection coverage.

JSON schema export includes table and column facets together with native objects
and their captured coverage. Unsupported input refuses the whole conversion.

## Offline index and stream declarations

The `values.convert` service also converts index, changefeed, and standalone
topic properties between separate typed desired and observed models. It works
without an open session, retains exact owner identity, and returns no prefix
when any value in a mixed batch fails. Conversion establishes no inspection
coverage. These models do not yet advertise comparison or planning.

Index declarations retain the native index kind, ordered index/data columns,
and kind-specific settings. Each declaration includes complete requirements on
the server-owned implementation tables: ordered columns and keys, column
defaults, storage, partitioning policy, and access constraints. These are
requirements on the index implementation, not independent tables to create.
Missing implementation tables or columns refuse conversion. Physical shard
boundaries also refuse conversion; current splits are not creation settings.
The observed index model canonicalizes unspecified implementation storage as
row storage and excludes internal sequence positions. This changes its definition
digest; this provider is pre-v1 and carries no reader for the previous shape.

Topic declarations preserve partitioning policy, retention, codecs, write/read
quotas, attributes, metrics settings, and consumer configuration. Consumer
settings retain read timestamps, availability periods, codec preferences,
attributes, and streaming-consumer presence. Access uses the same direct-grant
and effective-permission requirements as row tables. Runtime partition topology
and statistics cannot enter a declaration.

Changefeed declarations include the child topic's settings as well as mode,
format, virtual/resolved timestamps, attributes, AWS region, schema-change
reporting, and user SIDs. `disabled` and `initialScan` are independent settings.
An observation in `STATE_INITIAL_SCAN` converts to enabled intent with
`initialScan: true`. Converting that declaration back predicts
`STATE_ENABLED`; it does not prove that a server finished scanning. Scan
progress counters remain excluded.

Unknown fields/enums, runtime state in declarations, null settings, duplicate
names, and incomplete child descriptions are refused. Empty index/changefeed
sets keep their own desired model definitions. Indexes, implementation tables,
changefeeds, and consumers normalize by name; column and key order is retained.

## Offline native-object declarations

View declarations retain query text and table-service attributes, including
comments. The view and table APIs must agree about access metadata. Conflicting
access or additional table settings refuse conversion rather than creating
competing declarations for one view. The table API returns disabled partitioning
switches for views on both tested YDB lines. This exact placeholder is excluded;
other partition settings are refused.

External-source declarations preserve source type, location, and exact native
property keys and values, including authentication references. External-table
declarations preserve the data-source path, location, content options, ordered
column names and types, nullability, and typed column settings. Conversion does
not contact an external service or resolve a secret.

Coordination conversion retains optional timing and mode settings together with
access requirements. Native unspecified settings remain omitted. A desired
`struct_name` cannot convert to an observation because it has no server
counterpart. A runtime-reserved coordination identity is not exportable as a
user declaration.

Column-storage declarations retain hash-column order, partition and data-channel
counts, local-index settings, and TTL tiers. Optional false values, disabled TTL,
and omitted TTL stay distinct. The same validation applies to both
representations; native IDs and shard addresses have no declaration field.

The native models use distinct desired definitions. This changes experimental
definition digests and the coordination change/operation contracts. This
provider is pre-v1 and has no reader for the previous shapes.

The live CI jobs build a pinned Ptah host in a separate checkout and run
`schema inspect --format json` with an explicitly trusted provider manifest.
They compare every exported model with SDK conversion and check that coverage
keeps every inspected model family. These tests exercise row tables, columns,
views, external objects, and coordination nodes. Local runs enable the same
checks with `PTAH_HOST_TEST_BIN` pointing to that host binary.

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
SDK normalization and conversion without opening a database session. Live column-table
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

## Native row-table creation

The SDK planner accepts captured CREATE lifecycles for row tables. It retains
ordered composite primary keys, native column types and nullability, TTL,
storage settings, column families, attributes, partitioning settings, bloom
filters, read replicas, and tiering. Column families and typed literal defaults
come from native column facets. Missing creation settings request server defaults.
The common and native primary key declarations must agree on order.

The planner can replace a complete common CREATE TABLE step in the shared graph.
Its replacement carries table and physical-path effects and forbids SQL
transactions. A refusal returns no successful planning prefix. Rendering emits
a native operation envelope and validates its safety metadata again.

Execution requires an existing parent directory and establishes absence through
its complete listing. An existing path is refused, including an existing table
with the same schema. The provider binds the session and absolute path, checks
its guarding leases, sends CreateTable once, and reads the result back. It
confirms columns, key order, requested settings, and the absence of unexpected
indexes or changefeeds. A failed reply or unconfirmed result remains UNKNOWN.
The native API provides no atomic compare-and-set; these checks do not establish
exclusive ownership against another schema writer.

Access requirements, temporary tables, indexes, changefeeds, sequence defaults,
column compression or encoding, and richer common constraints are refused.
Table removal and rebuild remain unsupported, and alteration covers column
families only (see below). An unknown populated
field cannot disappear during compilation. A mixed SQL/native batch is validated
before its first unit executes.

Subprocess tests cover creation, inspection, stale plans, retained table data,
and refusal of a malformed later unit on YDB 25.1.4.7 and 26.2.1.14. Unit tests
exercise lost replies, lost confirmation, lease failure, and session cleanup.
The host still refuses general table planning through this provider: its common
table validation and comparison need to preserve complete native declarations
before that capability can be advertised. These SDK checks do not establish a
complete native CLI table migration workflow.

## Native column family alteration

The same planning service (`ydb-row-table-create`) accepts captured ALTER
lifecycles for row tables. It reads the declared and the observed table and
column properties, and plans one native AlterTable call that adds families,
changes the settings the declaration states, and moves columns between
families. Everything else about an altered table stays with the host: other
table settings, and the host's own column addition and removal steps, which the
plan leaves in place.

The rules follow the host's family rules:

- A setting the declaration leaves out is kept as the table holds it. A cluster
  profile gives new tables settings of their own, so the table is not reset.
  A declared `COMPRESSION_NONE` matches a family that reads back unspecified.
- A family only the table holds stays. No family is dropped, because YDB has no
  way to drop one.
- A column moves only when a column property record names it. A column with no
  record keeps its family. An empty family in a record means the default family.
- A key column stays in the default family. A column that names a family
  neither the declaration nor the table has is refused. So is a `keep_in_memory`
  that only one side sets, and a declaration that states families when the
  table's current families were not captured.
- A column the plan removes is never named, so it is never moved.

Ordering against the host's steps is explicit: the family step runs after the
addition of every column it moves, and before every column removal. A table
whose families already match yields no step and receipts that say so.

Execution reads the table first and requires every touched family and moved
column to match the plan, and every family to add to be absent. A table that
changed since planning is refused before the call. After the call the provider
reads the table back and checks that the requested families, settings and
placements hold and that nothing else moved: other families, other columns'
families, column names and types, and the key. A failed reply or unconfirmed
read remains UNKNOWN and is never retried. As for creation, these checks do not
establish exclusive ownership against another schema writer.

Live tests alter a table on YDB 25.1.4.7 and 26.2.1.14, plan again from the read
back to show the declaration no longer changes anything, check that rows
survive, and check that a stale alteration is refused.

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
Remaining work includes more source formats, comparison, table alteration and
removal, and planning, rendering, and operation models for other objects,
capability detection, environment credential sources,
monitoring, and dev-database provisioning. The provider advertises implemented
inspection, normalization, coordination YAML source decoding, comparison, planning,
rendering, SQL, native coordination and row-table creation execution, transaction, history, metadata,
lock, and offline SQL analysis services. The remaining capabilities stay
unadvertised.

The host's embedded implementation still needs removal when these operations
are wired through this executable. Provider release workflows, default Ptah bundling,
and independent installation acceptance remain pending. No v1 stability or
full YDB support is claimed by this development state.
