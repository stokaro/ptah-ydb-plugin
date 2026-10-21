# Ptah YDB provider

This repository owns the executable YDB provider for [Ptah #4204](https://github.com/stokaro/ptah/issues/4204).
It is under development. Inspection covers row tables and their indexes and
changefeeds, topics, views, coordination nodes, external data sources, and
external tables. Schema planning and migration execution are not implemented yet.

The executable opens native YDB connections and reads row-table schemas
through the public Ptah SDK. It supports anonymous, URL user/password, and URL token authentication.
Session receipts contain the selected database and measured server version.
The implementation does not import the Ptah application module.

## Build and test

```sh
GOWORK=off go test -race ./...
GOWORK=off CGO_ENABLED=0 go build -o bin/ptah-plugin-ydb ./cmd/ptah-plugin-ydb
```

The executable requires the SDK's authenticated local subprocess bootstrap.
Run it through a Ptah SDK client rather than invoking it as a command-line tool.
The SDK dependency is pinned to a published experimental revision. There is no
`replace` directive or workspace dependency.

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
host-module imports and local module replacements. These checks cover sessions, native row-table observations, offline
normalization, index implementation tables, changefeed consumers and retention,
standalone native objects, exact-directory completeness, and refusals for
incomplete reads. They do not establish
planning or migration support.

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
The complete scope includes typed schema objects and facets, offline source
formats, normalization, comparison, planning, rendering, execution, migration
transactions/history/locks/logs/tags, capability detection, environment
credential sources, monitoring, and dev-database provisioning. The provider advertises only implemented inspection and normalization
services. The remaining capabilities stay unadvertised.

The host's embedded implementation still needs removal when these operations
are wired through this executable. Provider release workflows, default Ptah bundling,
and independent installation acceptance remain pending. No v1 stability or
full YDB support is claimed by this development state.
