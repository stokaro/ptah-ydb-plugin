# Ptah YDB provider

This repository owns the executable YDB provider for [Ptah #4204](https://github.com/stokaro/ptah/issues/4204).
It is under development. Inspection covers row tables and their indexes and
changefeeds, column tables, topics, views, coordination nodes, external data
sources, and external tables. Native SQL execution and session-bound data transactions are
implemented, together with provider-owned migration history, event journals,
tags, and coordination locks. Offline YAML and Go annotation decoding is available for coordination-node declarations, and
YAML and Go annotation decoding for the partitioning, TTL and column families of row tables, and in Go the columns of each family.
Offline comparison, planning, and rendering produce native coordination
operations. Native row-table creation and alteration of column families, columns, the TTL and the automatic partitioning settings are available through captured SDK planning requests.
Execution verifies captured preconditions and reads back the result.
Offline migration lint splits YQL migrations and checks YD rules; see
[Migration lint](#migration-lint).

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
release carries one archive per platform, two JSON documents and a
`checksums.txt` file:

```text
ptah-plugin-ydb_<version>_<os>_<arch>.tar.gz   (.zip on Windows)
ptah-provider.json
release-lines.json
checksums.txt
```

`<version>` is the tag without the leading `v`. The platforms are linux,
darwin and windows on amd64 and arm64. Every archive holds the
`ptah-plugin-ydb` binary (`ptah-plugin-ydb.exe` on Windows), `README.md` and
`LICENSE`. The binaries are built with `CGO_ENABLED=0` and `-trimpath`.

`ptah-provider.json` names the provider, the release version, the oldest
provider contract a host must implement to start it (`min_contract`, a major and
a minor version), its engines and the archive and executable for each platform.
A host starts the release when its contract has the same major version and a
minor version at least `min_contract.minor`. The document states the minimum the
provider declares in its `Describe` reply, so the two cannot disagree. This
provider declares 25.0: everything it uses from a later minor is negotiated, so
a host of contract 25.0 keeps every service it serves. `ptah providers install`
reads the document to choose the newest release the host can start, without
downloading an archive. `release-lines.json` lists the release lines the provider declares.

`checksums.txt` lists the SHA-256 digest of every archive and of both documents
in `sha256sum` format (`<hex digest>  <file name>`). A host checks the archive
it downloads against the digest listed there.

Pull requests that touch the release inputs run `goreleaser check` and a
snapshot build, then verify the snapshot's `checksums.txt` and that
`ptah-provider.json` names exactly the archives GoReleaser wrote. To try it
locally:

```sh
GOWORK=off goreleaser release --snapshot --clean
(cd dist && shasum -a 256 -c checksums.txt)
```

## Build and test

The module pins `provider-sdk` v0.5.0, which speaks provider contract 25.3.
Builds require neither the Ptah application source nor a local SDK checkout.

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

The host and provider are built against the published SDK pin with
`GOWORK=off`. The live tests run twice per change and nightly. The blocking
run builds the newest published Ptah release whose provider contract this
provider can run on (`scripts/resolve-host.sh`); it is skipped with a notice
while no release speaks that contract. A second, non-blocking run builds Ptah's
master branch, so a host change that breaks this provider shows up early. This
repository owns the YDB end-to-end, live and parity tests; Ptah's own CI checks
the provider contract only on its in-tree fixture provider. These are source-build checks; packaged installation and
default bundling remain separate requirements.
Host `schema inspect --format json` exports the native desired model, including
provider-owned values and coverage. Common collections use names such as
`Tables`, `Fields`, and `Indexes`. This JSON is diagnostic output, not a schema
file input. Full native CLI planning remains pending.

`TestHostAppliesAndAltersARowTable_HappyPath` drives `ptah schema apply` of the
current host through this provider: it creates a row table from a Go source,
finds nothing to change on the next run, sets a TTL in place, and finds nothing
to change again. The run passes `--tx-mode none`, because the host refuses a
provider step that forbids a transaction under the default mode.

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
defaults, as in the host's embedded engine. Without the endpoint the session
reports the defaults of every supported line.

| Capability | Flag | Default |
| --- | --- | --- |
| `tiered_ttl` | `EnableTieringInColumnShard` | `false` on every line from 25.1 to 26.2 |

The host maps more flags than these. The provider reports only the
capabilities it plans with, because the host plans with the others itself: an
`index_rename` read as true would make the host plan a `RENAME INDEX` that the
provider's alteration does not carry out. `internal/featureflags/testdata`
holds the pages the mapping is checked against, and a live test turns the
flag on and off through the server's dynamic configuration and reads the
capability each new session reports.

Session close releases both the SQL pool and its parent native driver, even
when the RPC context is canceled. Cleanup results are retained for repeated
closes. A process accepts at most 1,024 lifetime session handles; start a new
provider process when that bound is reached.

## Dev realms

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
realm in place of any it named. Releasing the lease removes the realm with
everything in it, and then `ptah_dev` when no realm is left in it; an object
there is no statement to drop for stops the removal before anything is dropped.
The provider also removes the realms it holds when its process ends.

The session receipt reports the realm's path as its database, and inspection,
locks, migration history, migration metadata and every native write resolve
against it, so the realm reads as an empty database. Every SQL unit starts with
`PRAGMA TablePathPrefix` naming the realm, which resolves a relative path inside
it. The pragma does not confine an absolute path, a path that climbs out with
`..`, a name written through a `$` expression, or an object the whole database
shares, so the provider reads each unit before it dispatches any of them and
refuses the batch with `ydb.realm-escape` when a statement writes outside the
realm, moves the prefix, runs code it computes, or creates a user, group,
permission, resource pool or backup. An external data source or table, an async
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
database as not clean. One difference from the host is deliberate: the host
drops the lock node at the root of a realm, and the provider keeps it, because a
provider session holds its locks there.

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
database `local` without a credential, as local-ydb does. Parameters are merged over the default `go_balancer=disable`. Those that
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
use their native services. Dispatched writes are not retried.
Request IDs correlate calls and do not provide exactly-once execution.

Results distinguish a confirmed prefix from an uncertain remainder. A failed
dispatched unit reports UNKNOWN, even when earlier units succeeded. A unit may
contain several SQL statements, so its error cannot prove that none ran.
Cancellation between units reports the confirmed prefix. Diagnostics omit raw
driver errors and SQL text.

The provider offers `units.statement-timeout`. A host that lists it sends a
migration's statement timeout with each Execute call, and the provider bounds
each unit with it and answers for what YDB does when it fires, as the host's
embedded engine does (`migration/migrator`, `ydb_timeouts.go`); the reply says
the timeout stopped the unit. A unit in the caller's transaction is canceled
and the transaction never commits, so nothing of the unit is applied
(`NOT_APPLIED`, or `PARTIAL` after a prefix), and the provider rolls the
transaction back. An index added to a table that exists, and a column added
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

Coordination, row-table creation and row-table alteration operations use native envelopes. Other
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
Shared topic consumers, unextracted native object kinds, and
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
Comparison and planning remain limited to coordination nodes, the TTL, partitioning settings and column families of row tables, and the storage of
column tables; source adapters for the other families remain pending, as described under row-table declarations below.

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

The same service decodes the host's Go directive `ptah:schema:coordinationnode`,
written on a struct of its own, one node per directive:

```go
//ptah:schema:coordinationnode name="limits" schema="app" self_check_period="PT2S" read_consistency_mode="strict"
type Limits struct{}
```

`name` is required, and the other attributes are the YAML settings above, with
the same rules; `struct_name` has no attribute, since the host names the struct.
A YAML document that writes `coordination_nodes`, and every Go source, describes
the nodes completely, as they do for the host's embedded engine: a node the
source does not declare is one the plan removes from the directories it reads.

The subprocess tests exercise decoding, normalization, and representation
conversion without a database address. An HCL adapter remains pending (it needs
the HCL claims of provider contract 25.3); a YQL schema declares coordination
nodes through [SQL schema reading](#sql-schema-reading).

## Offline row-table declarations

The same source service decodes the table-scope settings that the host's
bundled YDB source reads from YAML and from Go annotations, so a table written
the way users write tables reaches the provider. The host lexes the source and
sends the values of the keys, directives and attributes the provider claimed;
the provider owns their meaning. The host refuses a YAML key the provider does
not claim. It reads a Go comment that no provider claims as no declaration, and
refuses an attribute of its own directives that no provider claims, so the
provider claims the Go directives and attributes it cannot carry and refuses
them by name. Nothing is dropped in silence.

A YAML table entry takes these keys, spelled as the YDB settings are:

```yaml
tables:
  documents:
    auto_partitioning_by_size: ENABLED          # ENABLED or DISABLED, in either case
    auto_partitioning_partition_size_mb: 2048   # a whole number of at least 1
    auto_partitioning_by_load: ENABLED
    auto_partitioning_min_partitions_count: 2
    auto_partitioning_max_partitions_count: 16
    read_replicas_settings: ANY_AZ:2            # PER_AZ:<n> or ANY_AZ:<n>; a count of 0 removes replicas
    key_bloom_filter: ENABLED
    column_families:
      default: {compression: lz4}               # off or lz4
      cold: {data: hdd, compression: lz4}       # data is a storage pool kind, kept as written
    platform:
      ydb:                                      # the TTL, spelled as the Go platform properties without the prefix
        row_deletion_column: created_at
        row_deletion_interval: P30D
```

The provider claims the `ydb` group of the table's `platform` key as
`platform.ydb`, and the host keeps no copy of the group. The provider therefore
refuses every key of the group other than `row_deletion_column`,
`row_deletion_interval` and `row_deletion_unit`. The host refuses a group that
holds a list or a mapping before it reaches the provider; scalar values arrive
as written.

In Go, the same settings are attributes of the host's own `ptah:schema:table`
directive, spelled as the YAML keys are, and a TTL is written as the platform
properties of the `ydb` engine. A family is a directive of its own,
`//ptah:schema:columnfamily`, on the table's struct, with the attributes `name`,
`data`, `compression` and `fields`. The `table` attribute names another table of
the file (`name` or `schema.name`) and wins over the struct's own table.
`fields` lists the columns the family holds, as a comma-separated list of the
column names (`fields="body, notes"`). The provider returns one column facet for
each name, owned by the column under the table, and the host attaches it to the
field of the struct the directive is written on. A family that has `fields` and
a `table` attribute for another table is refused, because the host places only
the columns of the directive's own struct. The default family lists no column,
a column sits in one family, and a name the struct has no field for is refused by
the host.

```go
//ptah:schema:table name="documents" auto_partitioning_by_load="ENABLED" read_replicas_settings="ANY_AZ:2" platform.ydb.row_deletion_column="created_at" platform.ydb.row_deletion_interval="P30D"
//ptah:schema:columnfamily name="default" compression="lz4"
//ptah:schema:columnfamily name="cold" data="hdd" compression="lz4" fields="body"
type Document struct { /* ... */ }
```

A TTL needs `row_deletion_column` and `row_deletion_interval`. The interval is an
ISO 8601 duration of weeks, days, hours, minutes and seconds (`P30D`,
`P2W3D`, `PT1H30M`), and YDB keeps whole seconds, so a fraction of a second,
months and years are refused. A column that counts since the Unix epoch also
needs `row_deletion_unit`, one of `SECONDS`, `MILLISECONDS`, `MICROSECONDS` and
`NANOSECONDS` in any case. Which column types fit the unit is checked when the
plan is made, where the table's columns are known.

All the keys, attributes and directives of one table become one desired
row-table facet, because a document may hold only one facet of a kind for a
table. The facet holds only what the source states. It has no primary key, which
comes from the table's common columns; a stated key must still be distinct and
ordered. A partitioning setting a source leaves out keeps what the table holds.
A table that declares nothing, such as an empty `column_families` mapping or
`store="ROW"` alone, gets no facet.

The values are read as the text the author wrote, so `3` and `"3"` are the
same count, and a boolean where `ENABLED` is due is refused. The shared
frontend normalizes the spelling of a YAML number before the provider sees it
(`1_000` arrives as `1000`, `0x10` as `16`); the bundled decoder reads the
written text and refuses both.

A starting layout is the partitions a new table is created with, and YDB takes
it only when it creates the table. `uniform_partitions` takes a count of at
least 1, and `partition_at_keys` takes split points in the host's grammar: a
comma separated list whose items are a value, for a split on the first key
column, or a parenthesized list of values for the leading key columns, such as
`10, 20` or `(10, 'a'), (20)`. A value is a bare run of characters or a string in
single or double quotes with backslash escapes. Both keys are claimed in YAML
and in the Go table directive, and a table that names both is refused where it
is decoded; the host accepts the pair when it decodes it and refuses the table
when it plans it. The key column types are checked when the table is created
(see "Table creation"). The corpus holds the inputs and the host's answers.

Not read yet, and listed so that nobody mistakes the silence for support:

| Setting | Why |
| --- | --- |
| `fields` of a column family in YAML | The host attaches a YAML value to a table, never to a column, so a YAML family cannot place columns. Claimed and refused by name; write the family in a Go directive. |
| `fields` with a `table` attribute for another table in Go | The host places the columns of the directive's own struct only. Claimed and refused by name. |
| `cache_mode` of a column family | The table service reports a cache mode in a family field that the pinned `ydb-go-genproto` lacks, and the provider refuses to read a table that holds one (`unknown fields in Ydb.Table.ColumnFamily`). It cannot be compared or set. Claimed and refused by name. |
| `changefeeds` in YAML | The comparison and planning of changefeeds do not exist yet. Not claimed, so the host refuses the key. |
| `ptah:schema:changefeed`, `ptah:schema:changefeed:consumer` in Go | The same. The host reads a Go comment that no provider claims as no declaration, so the provider claims both and refuses them by name. |
| Index partitioning, local, full-text and vector options | The row-table model holds no index option, and the host's bundled source writes the local and full-text options into the common index, which a claim cannot reach. The attributes of `ptah:schema:index` are claimed and refused by name. YAML has no index key claim. |
| Topics, resource pools, secrets, external objects, replications and transfers | Document-scope objects; the provider reads only `coordination_nodes` at document scope. |

A table is managed for its TTL when its source states table properties for it,
or when the host reports complete knowledge of the table properties of the
source (the knowledge a Go or YAML source carries when its format describes
them). A comparison that finds a TTL on such a table and none declared reports a
change, and the planner removes the TTL, as the bundled source does: measured
with the host at the commit named in the corpus, `schema apply` over a Go file
with no TTL plans `ALTER TABLE ... RESET (TTL)` for every table of the file. A
source that could not describe the properties, or a table whose current
properties were not read, leaves the TTL alone.

### Column tables

A column table is declared with `store="COLUMN"` in the Go table directive, or
with the `column_store` mapping in YAML. The Go attributes `partition_by_hash`
(a comma separated list of key columns), `column_shards` (a positive 32-bit
count) and `column_ttl`, and the YAML keys `hash_columns`, `partitions` and
`ttl`, state the hash key, the shard count and a tiered TTL. A tiered TTL names
a column, an optional epoch unit and ordered tiers; only the last tier may
delete rows, and every other one moves them to an absolute external data source
path. A TTL that only deletes is written as the row TTL
(`platform.ydb.row_deletion_*` in Go, the `row_deletion_*` keys of the YAML `ydb`
group) and becomes the column table's one deleting tier. The declaration
becomes one column-storage facet, and what it leaves out stays the server's
choice. `store="ROW"` declares nothing.

The corpus holds each spelling and each refusal with the host's answer
(`*-store-column-*` rows). The host's bundled decoders also accept a column
table beside row-table settings (partitioning, read replicas, a key bloom
filter, a starting layout, column families) and beside both a tiered TTL and a
row TTL, and its renderer refuses the table when it plans it. The provider
refuses such a declaration where it is decoded, with the renderer's words:
`row-table partitioning, column families and changefeeds cannot be applied to a
column table`, and `declare either tiered TTL or a TTL that deletes rows, not
both`. It is stricter in one place: the host reads a fractional YAML
`partitions` as its whole part, and the provider refuses it.

Inspection reads a column table, a `COLUMN_TABLE` directory entry, from the
table service and from the monitoring endpoint's scheme description
(`/viewer/json/describe`). The table service's description carries the
columns, the key, the hash key (`partitioning_settings.partition_by`) and the
TTL, and leaves out the shard count and the local indexes (measured on 25.1.4.7
and 26.2.1.14 for a table created with `AUTO_PARTITIONING_MIN_PARTITIONS_COUNT
= 3`); the monitoring page carries both. The two are read together and a
disagreement refuses the inspection. The table service writes a tiered TTL in
a field the pinned `ydb-go-genproto` lacks, so the provider reads a column
table's description with definitions of its own (`ColumnTableDescribeResult`,
adapted from upstream with its wire numbers). The table reads back as a common
table with one column-storage facet (hash key, shard count, data channel
count, local indexes and the TTL with its tiers, a TTL that only deletes
included) and a column facet for each column; it has no row-table, index or
changefeed facet. Its access metadata is not observed. A column table with a
column family, a column default, attributes or row-table settings is refused.
The hash function has to be the one YQL's `PARTITION BY HASH` writes,
`HASH_FUNCTION_CONSISTENCY_64`: the native `CreateTable` call writes
`HASH_FUNCTION_MODULO_N` instead, and the host's embedded engine reads no other.

Without the monitoring parameter in the address, an inspection that meets a
column table is refused with `column table <path> requires
monitoring=http(s)://host:port in the YDB address`, and the column table is
never read as a row table. The SDK client passes the host the status code of a
refused call and not its message, so the host reports `provider inspect failed
(FailedPrecondition)`.

Creation. The planner creates a declared column table with one YQL `CREATE
TABLE ... PARTITION BY HASH (...) WITH (STORE = COLUMN, ...)` through the query
service, once, outside a transaction, with the shard count as
`AUTO_PARTITIONING_MIN_PARTITIONS_COUNT` and the TTL as YQL writes it. YQL is
used rather than the native `CreateTable` call because the native call writes
`HASH_FUNCTION_MODULO_N`, which the host's embedded engine does not read. Every
key column is written NOT NULL, as the host writes it: YDB refuses a nullable
key column in a column table, and a Go field marked primary is nullable unless
it says `not_null`. The table is read back from the table service and the
monitoring endpoint and must hold the declared columns and key, the declared
hash key or, without one, the whole key, the declared shard count, the declared
TTL and no local index. A creation in a session whose address names no
monitoring endpoint is refused before anything is written. A tier that moves
rows needs the `tiered_ttl` capability, which a session reads from
`EnableTieringInColumnShard`; without it the plan is refused with the reason.
Column defaults, Serial columns, column families, indexes, local indexes and a
data channel count other than the server's 64 are refused.

Comparison and planning. The comparison `ydb-row-table-properties` compares the
column-storage facet of a table that exists on both sides, and reports a
`github.com/stokaro/ptah-ydb-plugin/column-storage-change` when the hash key,
the shard count, the TTL or the local indexes differ, or when the table is a
column table on one side and a row table on the other. A declaration that
leaves out the hash key or the shard count takes what the table holds. Under
complete coverage, a table the source states no column storage for asks for a
row table. YDB fixes a table's storage kind, hash key and shard count when it
creates the table and copies no column table into a new one, so the planner
refuses each of these changes with the host's words (`changing between row and
column storage requires an explicit data migration`, `changing a column
table's hash key or shard count requires an explicit data migration`, with both
values; `column-table rebuilds require an explicit data migration`). A change of
its local indexes and any column step of a column table are refused as not
planned yet.

The TTL changes in place, as the host's embedded engine changes it: `ALTER
TABLE ... RESET (TTL)` removes the TTL the table holds and `ALTER TABLE ... SET
(TTL = ...)` installs the declared one, each a step of its own
(`column-table-ttl/<n>/reset` and `.../set`, the RESET placed early and ordered
before the SET). YDB keeps no external data source a TTL names, so the held TTL
lets go of its sources before their owner drops or replaces them, and the
declared one is set once they exist. A table without a TTL takes the SET alone,
and a declaration without one the RESET alone. A lone RESET drops the table's
TTL setting and a lone SET creates it; a pair alters it twice, because the host
refuses a provider step that creates what another provider step drops (it runs
every creation of a phase before every drop), and the dependency orders the
pair. The declaration is held to the
rules of a creation, and a tier that moves rows needs `tiered_ttl`. Each step
reads the table from the table service and the monitoring endpoint, refuses
before it writes when the table holds another TTL than the plan saw or when the
TTL to set reads a column that is absent, not first in the key or of a type its
mode does not read, runs its statement once through the query service and reads
the TTL back.

`internal/tablesource/testdata/corpus` holds the inputs and the answers of the
host's bundled decoder for them, and the differential test in
`internal/tablesource` requires the provider to mean the same as the host for
every input the host accepts, to refuse every input the host refuses, and to
name every difference in the table above. `testdata/oracle` explains how the
answers are regenerated against a host checkout.

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
An incomplete desired source preserves known current configuration. Every
decision is made for one node, so a namespace the read left out produces no
diagnostic of its own.

The host names a declared node without a database, because it lives in the
session's, while inspection names that database in every owner and region it
reads. The comparison reads both in the host's terms, so the declared node and
the node read at its path are one node. A change names the node as the host
sent it: by the declared identity, or by the read one for a removal. A
comparison whose read names two databases is refused.

Coordination exports retain owner, direct grants, and effective-permission
requirements in an optional `access` field. Its JSON field names use snake case.
Omission leaves access unmanaged. The YAML source grammar above does not accept
access declarations. Comparison reports unsatisfied access requirements as
undecided; change/operation normalization refuses them. Existing nodes can
change configuration while satisfying unchanged access requirements. Creation
with explicit access requirements is refused because the coordination service
cannot promise inherited permissions or apply grants.

The comparison refuses contradictory identities, forged normalization receipts,
and collisions with declared relations. Required nodes are not removed.
Exclusions for this model match exact relative paths such as `app/workers`,
without case folding or trimming. The comparison leaves the database-root
`ptah_locks` node out, as the host's embedded engine leaves it out of every
read: it plans no removal of it and reports nothing about it. A declaration at
that path is the one case it reports, as undecided. Offline conversion and host
schema export retain this observation, including its configuration and access
metadata. Export does not authorize management: YAML and Go declarations and
change normalization still reject this root path.
This changes the desired model definition; pre-v1, no compatibility with the
earlier experimental definition is owed.

Offline subprocess tests cover decode, compare, and change normalization without
opening a session. Live tests compare native observations after closing their
session on both qualified YDB lines. `TestHostAppliesACoordinationNode_HappyPath`
drives `ptah schema apply` of the current host through this provider from a
YAML, a Go and a YQL source: it creates a node, finds nothing to change on the
next run, changes its settings in place, and removes it once the source no
longer declares it, reading the node back after each run.

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
settings. A starting layout (`uniformPartitions`, or `partitionAtKeys` as lists
of source text values) is a creation setting of the desired model only: a read
finds none, and converting a desired table to the observed representation leaves
it out. Partitioning policy remains declarative. Statistics and transient
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
coverage. Only the index model advertises comparison and planning, for adding
and dropping global secondary indexes of an existing row table (see
[Index alteration](#index-alteration)); the changefeed and topic models do not.

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

Fixture replay covers YDB 25.1 and 26.2 captures: monitoring pages copied from
the host, and pairs of a monitoring page and a table-service description read
from one live table each (`internal/columnstore/testdata/README.md`). A
subprocess test checks SDK normalization and conversion without opening a
database session, and live tests inspect column tables on both lines.

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
transaction requirement. The path effect names no database, as the host names
a path of the session's database. Table-to-node and node-to-table replacements
require an explicit common path handoff. Unsupported common footprints and
overlapping parent/descendant paths are refused until a directory lifecycle is
captured. The host's window markers (such as `common/feature-creations`) and the
column of a column step occupy no path, so they order nothing; the host places
the step between its markers. A common step that names another database than
the change is refused. The parent directory must already exist; execution never
creates or deletes it.

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
batch refusal, and lease loss on both tested YDB release lines. The host routes
a coordination node's comparison, planning and execution to these services
(see [Coordination comparison](#coordination-comparison)); other object
lifecycles remain pending.

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
refuses, so validation accepts them. Table paths that coincide or nest are refused
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
agree on order.

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
families, the addition and removal of columns, the TTL and the automatic partitioning settings (see below). An unknown populated
field cannot disappear during compilation. A mixed SQL/native batch is validated
before its first unit executes.

Subprocess tests cover creation, inspection, stale plans, retained table data,
and refusal of a malformed later unit on YDB 25.1.4.7 and 26.2.1.14. Unit tests
exercise lost replies, lost confirmation, lease failure, and session cleanup.
With the validation service in place, `ptah schema plan` through this provider
gets past validation and stops at the host's common-step stage ("A common step is
outside the supported table creation lifecycle"), because the host's common table
comparison does not yet preserve complete native declarations. These SDK checks do
not establish a complete native CLI table migration workflow.

## Native table alteration

The same planning service (`ydb-row-table-create`) accepts captured ALTER
lifecycles for row tables. It owns five facets of an altered table: its column
families, the addition and removal of its columns, its TTL, its automatic
partitioning settings, and its global secondary indexes. Each becomes native
AlterTable calls. Everything else about an altered table stays with the host:
other table settings, and any column step the native call cannot state. The
service also claims the changefeeds of an altered table and keeps them as they
are: the host plans no table that holds a facet no planning service claims, and
the comparison reports no changefeed change.

### Column families

The service reads the declared and the observed table and column properties,
and plans one call that adds families, changes the settings the declaration
states, and moves columns between families. The rules follow the host's family
rules:

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

### Partitioning settings

The service changes the automatic partitioning settings of an existing row
table with one native `AlterTable` step, `row-table-partitioning/N`. The step
carries only the settings that change, and also the whole partitioning the table
held when the plan was made. The settings are `AUTO_PARTITIONING_BY_SIZE`,
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
  and whose minimum differs from the table's is a change the planner refuses
  with `UNIFORM_PARTITIONS alter is not supported` (or the `PARTITION_AT_KEYS`
  spelling) and the two minimums. A layout beside a declared minimum leaves no
  witness and is not compared. The comparison reports the refused layout as a
  difference, so that the plan reaches the planner.
- A minimum count above a nonzero maximum is refused, checked against the value
  the table holds for a bound the declaration leaves out. A partition size beside
  disabled partitioning by size is refused too: the server turns partitioning by
  size on when a size is set, so the declaration has to state both.
- When the step switches partitioning by size on, it names every setting the
  declaration states, because the server resets the size and the minimum count
  the call leaves out.
- Impact is behavioral: the settings decide when YDB splits and merges
  partitions. The step reads the table and alters its partitioning only, and does
  not depend on the family, column or TTL steps.

### Index alteration

The service adds and drops global secondary indexes of an existing row table.
Each index is one native `AlterTable` step: `row-table-add-index/N` reads the
table and creates one index object, `row-table-drop-index/N` reads the table and
drops one. The index object is a host subject of kind `index`, named by the
index and parented by the table, so two index steps of one table never share an
effect. An addition is `ADD INDEX name GLOBAL [SYNC|ASYNC] [UNIQUE] ON (keys)
[COVER (columns)]`; a removal is `DROP INDEX name`. Impact is additive for an
addition and destructive for a removal.

The declaration is the index set model (`table-indexes`, desired side) as a
facet of the table, beside the table properties. It is managed when the
declaration states it: an index the table holds and the set leaves out is
dropped, and a declaration with no index set leaves the indexes alone. The
comparison reads the specification of each declared index: the name, the kind,
the ordered key columns and the ordered covered columns. The declared
`implementation` requirements on the server-owned index table are part of the
model and have to be valid, but they are not compared and not sent: YDB builds
that table itself.

- YDB cannot alter an index in place. An index the table holds under a declared
  name with another kind, other key columns, other covered columns or another
  key order is a change the planner refuses with a diagnostic that names both
  definitions. It does not plan a drop followed by an addition, because that
  rebuilds the index. Declare the removal and the addition in separate plans if
  a rebuild is wanted.
- Only global sync, async and unique indexes are altered. A vector or fulltext
  index, and an index that states settings of its own (such as the partitioning
  of its table), is refused by name when the plan would have to add it, drop it
  or tell it from the held one. The same index on both sides, unchanged, is no
  difference and needs no support.
- A unique index needs a server that accepts `ALTER TABLE ... ADD INDEX ...
  GLOBAL UNIQUE`; YDB 25.1 refuses it unless `enable_add_unique_index` is set.
- An index reads columns the declaration states. A declared index that reads a
  column the declaration does not state is refused, and so is a kept index whose
  column the declaration drops.
- Ordering against column steps of the same table, whoever carries them out: an
  index is added after the addition of every column it reads, and dropped before
  the removal of every column it reads or covers. A column removal step that does
  not name its column is assumed to remove any column. Execution reads the
  indexes of the table before dropping a column as well, so a column an index
  still reads is refused before the call.
- YDB builds a new index in the background. The provider waits for the index to
  read back as ready, bounded by ten minutes, before it confirms the step.
  Inspection refuses an index that is not ready, so a table with an index in
  the build is never compared.

### Execution

Execution reads the table first. A family operation requires every touched
family and moved column to match the plan, and every family to add to be absent.
An addition requires the column to be absent. A removal requires the column to
exist and not to be a key column, an index column or stored column, the TTL
column or a partitioning column; the server refuses each of those only after the
call, which would leave the outcome uncertain, so the provider reads them first.
A TTL operation requires the table to hold exactly the TTL the plan captured, or
none, and a TTL to set must read a column the table holds with a type its mode
reads. A partitioning operation requires the table to hold exactly the
partitioning settings the plan captured. An index addition requires the index
to be absent and to read columns the table holds, and an index removal requires
the table to hold the index as the plan captured it. A table that changed since
planning is refused before the call.

After the call the provider reads the table back and checks that the requested
change holds and that nothing else moved: other families and columns, column
names and types, their order, the key, and the TTL. The TTL must read back as
the one the request sets, run interval included, or as none after a removal, and
as before for every other operation. After an index operation the added index
must be ready and hold the requested kind and columns, a dropped index must be
gone, and every other index must read back unchanged. After a partitioning operation the table
must hold every setting the request names and the partition columns it held; the
settings the server derives from them, such as the partition size it no longer
reports once partitioning by size is off, are not compared. An added column must read back with its
type, nullable and in the default family. A failed reply or unconfirmed
read remains UNKNOWN and is never retried. As for creation, these checks do not
establish exclusive ownership against another schema writer.

Live tests alter a table on YDB 25.1.4.7 and 26.2.1.14: families and column
placements; adding columns, placing one in a family and dropping others in a
single plan; changing each partitioning setting on its own and together,
including switching partitioning by size off and on again; setting, changing, moving and removing a TTL, including the move of
a TTL to an added column in the same plan that drops the column it read; and the
refusal of an indexed column, of a TTL column the server would refuse, and of a
stale alteration before the call; and adding and dropping sync, async, covering
and unique indexes, with the refusal of a changed index definition. They plan again from the read back to show the declaration no
longer changes anything, and check that rows survive.

### Table property comparison

The offline `models.compare` service `ydb-row-table-properties` compares the
table and column properties of a row table that exists on both sides. It reports
one `github.com/stokaro/ptah-ydb-plugin/row-table-properties-change` for a table whose TTL, partitioning settings or column families
differ from the declaration, one for each column that sits in another
family, and one for a table whose index set differs from the declared one: an
index added, removed or changed. An index change carries both complete index
sets and names them with a `facet` key, so a table can have a property change
and an index change at once. The comparison applies the rules of the alteration planner, so a
difference it reports is one the planner acts on, and a table that differs only
in its TTL, its partitioning settings or its families still reaches the planner. A change carries both
complete facets, replaces no owner, removes no state and leaves access
unchanged. A facet present on one side only states nothing to compare; the
planner decides what the missing read means. The planner accepts exactly these
changes, accounts for each one with the steps of its table alteration, and
refuses every other change kind.

The host names a table and its columns without a database, because they live in
the session's, while the provider's inspection names that database in the owner
of every facet it reads. The comparison and the planner read a facet whose
subject names no database without its owner's, so both sides of one table name
one owner. A facet whose owner is another table than its subject names is
refused.

## SQL schema reading

The provider serves `schema.read-sql` for the `ydb` engine in document mode.
`ptah schema plan` and `ptah schema apply` hand it the documents of a
`--schema-file` or of a schema directory of `.sql` files, and it reads them as
one load without a database session. The reader is a port of Ptah's own YQL
schema reader at commit `a4e286dbf5ac42a609bd6ccd2b60dcc3335c210d`, so a load
reads to the model Ptah's bundled YDB owner reads, and a load Ptah refuses is
refused with Ptah's words and at the position Ptah names.

The reply carries the tables with their keys, columns, defaults, serial
columns, comments and indexes; views; users, groups and memberships; grants and
revocations; and coordination nodes as this provider's coordination model. It
says what each document declares, traces every object to its statement, and
claims complete coverage of the row-table, column, column-storage and
coordination models. A `ptah:not-described` header entry of kind
`coordination_node` limits that claim.

A refused token is quoted with at most its first 64 bytes, where Ptah quotes it
whole: a schema file can hold a secret in a string literal.

A load that declares something no reply field or model of this provider can
carry is refused by name, with no partial reading:

- the partitioning settings, column families, TTL and column storage of a
  table, which Ptah refuses in a SQL schema reading;
- the partitioning and vector settings of an index;
- changefeeds, topics, external data sources and external tables, which this
  provider declares from no source document;
- a user's password;
- async replications, transfers, streaming queries, secrets, resource pools and
  resource pool classifiers, which this provider has no model for.

Some of what a reading carries is not planned yet. Table creation refuses
column defaults, indexes and table comments. Ptah refuses views and roles from
a provider's reading. A coordination node is planned as the coordination
services describe.

`internal/yqlschema/testdata` holds the differential corpus: every load of YQL
schema documents Ptah's own tests read, the YDB fixtures Ptah renders, and edge
cases, each with the answer Ptah's reader recorded. The unit suite compares the
provider's reading with each answer, refusal for refusal. The corpus README says
how the answers are recorded again.

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
terminators is refused because the SDK requires tokens for every
non-whitespace source range. No prefix of a refused batch is returned.

A request contains at most 256 sources and 3 MiB. Each source contains at most
65,536 significant tokens, and the complete response is bounded by 3 MiB.
Cancellation returns no analysis. The native CLI still requires integration of
YDB's table comparison rules; the provider does not claim the common table
identity contract to bypass that requirement.

## Migration lint

The provider serves the SDK's `lint.migrations` service for the `ydb` engine
without a database session. It publishes its rules as data, with the codes,
titles and severities of the host's built-in YDB owner, whether a rule also
reads the down half of a migration, and the shared rules a finding replaces on
the same statement.

A split reads each migration file as YQL, with the lexer the SQL analysis uses.
It returns every statement's byte range, line, words and the comments directly
above it, which the host reads suppression directives from. It also reports the
facts the host's shared rules read: a drop that loses state (a topic, a
transfer, a coordination node, a streaming query, a replication dropped with
CASCADE), YQL's unique index, the steps of a table rebuild, and a safety
judgment for the statements only YDB's owner recognizes. The splitter reads
what a rebuilt table holds from the directory itself: the up migrations of
lower versions in version order, then the statements of the file before the
rebuild. An annotation judges the statements a host split without a dialect,
and declines a statement YQL would split differently.

A check judges every file of a run under the rules the request enables. A rule
reads the statement text and the capability map the request carries, never the
server version. The rules that need to know what a table looks like before a
statement read it from the directory itself, as the splitter does: whether an
index or the TTL uses a column, a table's partitioning settings, the views that
read it, its changefeeds and column families, the Serial columns and sequence
restarts, failed-over replications, and the comments Ptah's COMMENT ON
statements left. A table created outside the directory is unknown and reports
nothing.

`internal/migrationlint/testdata` holds the host's answers for a corpus of
migration directories and requests, recorded at one host commit, and the tests
require the same statements, facts, annotations and findings, message for
message. Its README says how to record them again.

### Statement facts for `ptah sql lint`

For a host that lists `lint.statement-facts`, which the engine lists beside
`lint.migrations`, a split also says what each statement is, which table it
creates without a primary key, and which capabilities it needs. `ptah sql
lint` reads a YDB file through these facts when this provider serves the
engine, in place of the host's built-in YQL reading, and reports what that
reading reports:

- a CREATE TABLE or an ALTER TABLE is examined; any other CREATE, ALTER or
  DROP is a schema statement no rule examines, named by its first two words;
- any other statement changes no schema object and is named by its first word,
  or as a named expression;
- a CREATE TABLE without a PRIMARY KEY clause names its table, and a table
  written as a named expression keeps the name as written;
- the capabilities are those the migration lint rules read: a unique index or
  a column with a default added to an existing table, a vector index, and a
  vector index over bit vectors. Each names the index or the column and the
  clause that needs it. The host compares the key with the capabilities of the
  server version it lints for.

A statement that starts with no word, such as a string or a parenthesized
query, is an expression the linter does not lint, and the first statement of a
file that opens with a UTF-8 byte order mark is read like any other. The
built-in reading skips both and reports nothing, which lets such a file read as
clean; the provider is stricter on purpose.

`internal/migrationlint/testdata/statementfacts` holds YQL files with the
built-in reading's findings on three release lines, recorded at one host
commit. The unit tests and an end-to-end run of the host's `ptah sql lint`
through this provider require the same rules at the same positions about the
same tables and capabilities. Its README says how to record them again.

A host that does not list the feature is offered none of these facts, and an
annotation request that asks for them is refused.

The host owns output formats, severity overrides, suppression and the rules
every dialect shares. The host's built-in owner reports nothing from these rules
on a run that names no dialect; the provider is only asked about its own engine,
so a host does not send it the codes of such a run.

## Rows, row writes and seeds

The provider serves the row services of the contract (`data.type-rows`,
`data.read-rows`, `data.render-writes`) and the seed service (`data.seeds`),
with the answers of the host's embedded YDB engine: its row typing
(`internal/ydbdata`), its row diff statements (`migration/datadiff`) and its
seeds (`migration/seeder` and the YDB seed service). The host keeps which rows
are declared, the row diff, the ordering and the seed policy.

YQL types every literal and converts few of them, so every value is compared
and written in its column's YDB type: the live table's type where the column
exists, and otherwise the type the declared type lands on for the request's
capabilities (`VARCHAR(255)` is Utf8, `TIMESTAMP` is Timestamp64 with
`wide_date_time_types` and Timestamp without it, `DECIMAL(10,2)` needs
`parameterized_decimal`). `TypeRows` returns the declared and the live rows in
one canonical form, so a declared `12.50` pairs with the `1.5` a Decimal reads
back, a moment in another zone with the one in UTC a read returns, `PT26H`
with the interval of the same length and an upper-case UUID with the lower-case
one YDB stores. A Decimal is a decimal at its column's scale, a Date a date, the
other date and time types a timestamp, an Interval a count of nanoseconds,
String and Yson bytes, Json and JsonDocument JSON text (JsonDocument with sorted
keys and no spaces), and Utf8, Uuid and DyNumber text. A value its column
cannot hold refuses the request with the host's words, and a column no side
types keeps its values.

`RenderRowWrites` writes the host's statements: `INSERT INTO` with the columns
sorted by name, `UPDATE ... SET ... WHERE` with the columns outside the key
sorted and the key predicate in sorted order, `DELETE FROM ... WHERE`, and a
key column holding null as `IS NULL`. An UPSERT is YQL's `UPSERT INTO` in the
shape of the INSERT; the host's row diff writes none. A value a column cannot
hold, or a column the write names no type for, refuses the batch with
`UNSUPPORTED_VALUE` and a message that names the write and the column but never
the value.

`ReadRows` reads one page of a row or column table in the order of the
requested key columns and then of the primary key columns the request leaves
out, so the order is total, and resumes after the last row of a page through
its token. YDB orders a null first, and the cursor steps past nulls and equal
keys. A page holds at most the requested rows and 3 MiB. A missing table is a
complete page that says so.

A seed file runs as the queries YQL reads it into, in one serializable
transaction with its ledger row, so the file and its row commit together or not
at all. A transaction YDB aborts for a conflicting one runs again, at most five
times. A file holding a scheme statement, a BATCH UPDATE or BATCH DELETE, a
translation setting other than `--!syntax_v1` or a delimiter directive is
refused before anything runs, with the position of the statement, and so is a
statement that leaves the dev realm of a realm session. A file that hits an
existing key is `KEY_CONFLICT`, and nothing of it stayed. A failed commit that
YDB did not answer with a key conflict or an abort is `UNKNOWN`.

The ledger is the host's table `schema_seeds` at the root of the session's
database, created with the host's statement, so a ledger the embedded engine
wrote is read and extended here, and the other way round. A table of that name
with another layout is refused and never adopted. A missing ledger reads as
missing and is not created by a read.

The engine also declares the query-builder spelling the host's YDB descriptor
states (`query.dialect`): named parameters, YQL quoting with paths, unsigned
row bounds, `UPSERT INTO`, and the capabilities the builder assumes for YDB
when its caller names no target.

`internal/rowdata/testdata` holds row cases and the host's answers for them,
and `internal/yqlquery/testdata` YQL texts and the queries the host splits each
into; the README there says how to record them again. The one difference is
the wording of a write refusal, which names the value in the host's message and
never in the provider's. A live test writes rows of every value kind through
the services, reads them back in pages and types both sides into one form; the
seed tests run in a dev realm and read a ledger written with the host's
statements. Against a Ptah whose `schema apply` and `ptah seed` serve declared
rows and seed files through a provider, the end-to-end tests run both through
this one in a dev realm and read the table back.

## Embedding store

The provider serves the SDK's embedding store (`embeddings.store`, ADR 0032)
for the `ydb` engine, for a host that lists the feature too; the engine
declares the `immutable` consistency mode. YDB has no triggers, so the outbox
mode, which records each source change in the transaction that makes it, is not
offered. The host keeps the lifecycle of `ptah inference`; the provider stores
what the host decided and refuses nothing for a lifecycle reason.

The layout is the host's embedded YDB engine's (`internal/embedydb`), so a
database either one wrote runs on through the other:

- the run state is the four tables `ptah_embedding_generation`,
  `ptah_embedding_run`, `ptah_embedding_event` and `ptah_embedding_pointer` at
  the root of the session's database, with the host's columns, keys and
  nullability, a timestamp as Timestamp64 on 25.2 and newer and as Timestamp
  on 25.1; an existing table of another shape is refused with `NOT_OWNED` and
  never altered;
- a generation beside its source adds the vector column, a nullable `String`,
  and four `Utf8` columns named by the suffixes `_generation`, `_input_hash`,
  `_source_version` and `_state`; a chunked generation keeps a table of its
  own keyed by the source key and `<column>_chunk_ordinal`, marked as the
  generation's by its comment;
- a vector is stored as the bytes `Knn::ToBinaryStringFloat` produces, and the
  index is a `vector_kmeans_tree` with the metric's `distance` or `similarity`
  and the specification's `levels` and `clusters`.

A compare-and-write (`MutateEmbeddingState`, `CommitEmbeddingBatch`) is one
serializable YDB transaction that reads what it compares, compares the token,
writes and commits. A transaction YDB aborts, or a server that is briefly
overloaded or unavailable, runs again, at most twelve times; a commit that fails
any other way is `UNKNOWN`. A token is a digest of the records read, so a
record that changed or appeared gives another one. Written records come back as
a later read returns them: optional text empty, times in UTC to the
microsecond.

A batch makes each written key's rows equal to its set: on the source's
columns an `UPDATE` by the key, so a key the source no longer has creates no
row, and in a table of its own an `UPSERT` that selects the key out of the
source, with the rows past the set's end removed and a tombstone removing them
all. A write into a generation whose index exists is refused as `UNSUPPORTED`
on 25.2 and older, whose vector index answers a search as the table stood at
its build.

A source is paged by its primary key, which the specification's key fields
have to be. The verification walk reads both sides in one query per page, one
table read once or a full join of two, and never splits a key's rows across
pages. A search asks twice, through the index with `VIEW` and by exhaustive
scan. Building an index needs `vector_indexes`, which 25.1 has with
`EnableVectorIndex` on; the provider reads the flag where the address names the
monitoring endpoint. A drop checks ownership before anything goes: a table by
its mark, columns by their comment and by holding no other generation's rows.

`internal/embedstore/testdata` holds the host's answers for the run-state
tables on 25.1 and 26.2, a corpus of specifications and a set of vectors, and
the tests require the same statements, objects or refusals, and bytes. Its
README says how to record them again. The live tests run the store on both
lines in dev realms. Against a Ptah build they read a database the host's
embedded engine prepared, backfilled and indexed, search it through the
engine's index, and append an event the engine then reads; and against a Ptah
whose `ptah inference` takes `--provider-config` they run the host's YDB
inference flows through this provider: two generations from plan to cutover, a
rollback and a retirement, a chunked generation in a table of its own, the
refusals, and the answers an agent reads, which carry no source content.

## Extraction still required

The source reference is Ptah commit `7b4b19f49fadf03ce81def2f6168ea60b3261a71`.
Remaining work includes more source formats, comparison, the alteration of table
settings other than column families, table removal and rebuild, planning, rendering and operation models for other objects,
and the capabilities of the feature flags the host plans with. The provider advertises implemented
inspection, normalization, coordination YAML source decoding, comparison, planning,
rendering, SQL, offline schema validation, native coordination, row-table creation and row-table alteration execution, transaction, history, metadata,
lock, offline SQL analysis, SQL schema reading, migration lint, row and seed services, dev database cleanup, replay guard and embedding store services. The remaining
capabilities stay unadvertised.

The host's embedded implementation still needs removal when these operations
are wired through this executable. Provider release workflows, default Ptah bundling,
and independent installation acceptance remain pending. No v1 stability or
full YDB support is claimed by this development state.
