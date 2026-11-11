# Ptah YDB provider

This is the official, separately installed [YDB](https://ydb.tech/) plugin for [Ptah](https://ptah.run/). It reads YDB schemas, compares them with a desired schema, plans and applies supported changes, and runs migrations through Ptah's common workflow. The plugin owns YDB connections, YQL, catalog interpretation, native objects, and execution. Ptah itself contains no built-in YDB engine.

## Install

Install the newest provider release compatible with your Ptah executable, then check the installed copy:

```sh
ptah providers install ydb
ptah providers check --provider-engine ydb --json
```

A successful check reports `"status": "compatible"`. To choose a release explicitly, pass `--version 0.8.0` to `ptah providers install ydb`; the version has no leading `v`. A `ydb://` or `ydbs://` URL, or the `ydb` dialect, then selects the installed provider automatically. [Ptah's install guide](https://docs.ptah.run/edge/extend/executable-providers/#install-an-official-provider) explains release selection, verification, and explicit manifests.

## First inspection

Start a reachable YDB server with a database at `/local`. The example URL uses the local development server without credentials and disables endpoint discovery, so it also works through a port forward. Use your own database URL and authentication settings for another server.

```sh
ptah schema inspect --db-url 'ydb://localhost:2136/local?go_balancer=disable' --format sql
```

This prints the inspected schema as YQL. The output depends on the objects in your database. For connection parameters and TLS, see [Using the provider with Ptah](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/usage.md) and [Connection addresses](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/connections-and-execution.md#connection-addresses). The [schema and migration guide](https://docs.ptah.run/edge/schema/) covers the next steps in Ptah.

## Capabilities and limits

The provider handles row and column tables, indexes, topics, changefeeds, coordination nodes, external objects, resource pools, secrets, replications, transfers, streaming queries, and the YDB-specific parts of access control. It also implements migration history, locks, YQL analysis and lint, and offline source handling. Supported operations and declarations vary by object, source format, negotiated feature, and YDB release line. Start with the [object primer](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/objects.md), then use the reference pages below for exact behavior.

YDB schema changes are not transactional. An inspection or export can report incomplete catalog coverage or a format-specific loss; read those diagnostics before treating a file as a complete copy of a database. The provider's declared release lines are [in source](https://github.com/stokaro/ptah-ydb-plugin/blob/master/internal/backend/release_lines.go): CI exercises YDB 25.1 and 26.2, while the intermediate declared lines are best effort. Server features can further limit an operation.

## Server vulnerability scans

The provider advertises `security.vulnerability-scan` and `security.advisory-snapshot` at provider and YDB engine scope. It [bundles the reviewed YDB server advisories](internal/vulnerability/ydb.json) and sends them to Ptah with its engine description. Ptah reads `SELECT Version()` during connection setup and matches that version against the provider's snapshot. Neither process downloads advisories during a scan or inspects application dependencies. The [daily source check](.github/workflows/security-snapshots.yml) updates the snapshot through a review pull request.

For a self-managed YDB server, run `ptah db vulnerabilities --db-url "$DATABASE_URL"`. The default deployment is `self-managed`. If the service operator controls server patching, pass `--deployment managed`; Ptah then reports `not_applicable` and does not match server CVEs. The endpoint alone does not establish who patches the server. See the [vulnerability scan guide](https://docs.ptah.run/edge/operate/vulnerability-scan/) for source coverage and CI policy.

## Current status

The built-in YDB implementation was [removed from Ptah](https://github.com/stokaro/ptah/pull/5298). Official providers are [installed separately](https://github.com/stokaro/ptah/blob/master/docs/adr/0031-official-providers-are-installed-separately-and-ptah-pins-no-provider-release.md); they are not bundled with a Ptah release. The [parity tracker](https://github.com/stokaro/ptah-ydb-plugin/issues/206) is closed, and release archives and verification workflows are in use. Limits are described by operation in the reference pages, rather than as an unfinished extraction checklist.

The release's `ptah-provider.json` states its **minimum host contract**; the provider's [`Description`](https://github.com/stokaro/ptah-ydb-plugin/blob/master/internal/backend/provider.go) supplies the same minimum. The SDK version is pinned in [`go.mod`](https://github.com/stokaro/ptah-ydb-plugin/blob/master/go.mod). These are different from the Ptah release number and from features negotiated after startup. At this revision the minimum is contract **25.7**. A specific operation can require a newer host: for example, the [v0.8.0 transfer fix](https://github.com/stokaro/ptah-ydb-plugin/releases/tag/v0.8.0) needs Ptah v0.25.0 and contract 25.40. That requirement does not change the provider's startup minimum.

## Documentation

The [published YDB setup page](https://docs.ptah.run/edge/extend/providers/ydb/) is the entry point on docs.ptah.run. The source pages below are available in this repository immediately; the website imports them from a selected provider release.

| Task | Page |
| --- | --- |
| Connect and use Ptah or ptah-compat | [Usage](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/usage.md) |
| Understand YDB objects | [Object primer](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/objects.md) |
| Write Go annotations or YAML declarations | [Declarations](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/declarations.md) |
| Run migrations | [Migrations](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/migrations.md) |
| Use embedding generations | [Inference](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/inference.md) |
| Connections, execution, history, and catalog reads | [Runtime reference](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/connections-and-execution.md) |
| Offline models, comparison, and source conversion | [Schema model reference](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/table-models.md) |
| Native YDB object planning and execution | [Object lifecycle reference](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/native-objects.md) |
| Table and column changes | [Table lifecycle reference](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/table-creation-and-rebuild.md) |
| YQL, lint, rows, and rendering | [SQL and data reference](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/yql-reference.md) |

The [documentation index](https://github.com/stokaro/ptah-ydb-plugin/blob/master/docs/README.md) maps more specific topics. To build, test, or release the plugin, read [Contributing](https://github.com/stokaro/ptah-ydb-plugin/blob/master/CONTRIBUTING.md).

<details>
<summary>Former README section links</summary>

Detailed sections moved to the reference pages. These anchors keep existing links to this README useful; follow the corresponding reference link for the current text.

| Former section | Current location |
| --- | --- |
| <a id="releases"></a>Releases | [Contributor release reference](CONTRIBUTING.md#releases) |
| <a id="async-replications-and-transfers"></a>Async replications and transfers | [Open section](docs/streaming-and-workload-objects.md#async-replications-and-transfers) |
| <a id="build-and-test"></a>Build and test | [Open section](CONTRIBUTING.md#build-and-test) |
| <a id="changefeeds"></a>Changefeeds | [Open section](docs/native-objects.md#changefeeds) |
| <a id="column-addition-and-removal"></a>Column addition and removal | [Open section](docs/table-settings.md#column-addition-and-removal) |
| <a id="column-families"></a>Column families | [Open section](docs/table-settings.md#column-families) |
| <a id="column-tables"></a>Column tables | [Open section](docs/table-models.md#column-tables) |
| <a id="common-column-semantics"></a>Common column semantics | [Open section](docs/table-creation-and-rebuild.md#common-column-semantics) |
| <a id="common-objects"></a>Common objects | [Open section](docs/migration-state.md#common-objects) |
| <a id="comparison-counts"></a>Comparison counts | [Open section](docs/coordination-models.md#comparison-counts) |
| <a id="connection-addresses"></a>Connection addresses | [Open section](docs/connections-and-execution.md#connection-addresses) |
| <a id="coordination-comparison"></a>Coordination comparison | [Open section](docs/coordination-models.md#coordination-comparison) |
| <a id="coordination-locks"></a>Coordination locks | [Open section](docs/migration-state.md#coordination-locks) |
| <a id="dev-database-cleanup"></a>Dev database cleanup | [Open section](docs/connections-and-execution.md#dev-database-cleanup) |
| <a id="dev-databases"></a>Dev databases | [Open section](docs/connections-and-execution.md#dev-databases) |
| <a id="dev-realms"></a>Dev realms | [Open section](docs/connections-and-execution.md#dev-realms) |
| <a id="embedding-store"></a>Embedding store | [Open section](docs/data-services.md#embedding-store) |
| <a id="error-reasons"></a>Error reasons | [Open section](docs/catalog-and-errors.md#error-reasons) |
| <a id="execution"></a>Execution | [Open section](docs/indexes-and-table-execution.md#execution) |
| <a id="execution-and-transactions"></a>Execution and transactions | [Open section](docs/connections-and-execution.md#execution-and-transactions) |
| <a id="external-data-sources-and-external-tables"></a>External data sources and external tables | [Open section](docs/native-objects.md#external-data-sources-and-external-tables) |
| <a id="extraction-still-required"></a>Extraction still required | [Open section](#current-status) |
| <a id="feature-flags"></a>Feature flags | [Open section](docs/connections-and-execution.md#feature-flags) |
| <a id="index-alteration"></a>Index alteration | [Open section](docs/indexes-and-table-execution.md#index-alteration) |
| <a id="index-declarations-of-the-providers-types"></a>Index declarations of the providers types | [Open section](docs/indexes-and-table-execution.md#index-declarations-of-the-providers-types) |
| <a id="index-settings-from-a-source"></a>Index settings from a source | [Open section](docs/indexes-and-table-execution.md#index-settings-from-a-source) |
| <a id="migration-history"></a>Migration history | [Open section](docs/migration-state.md#migration-history) |
| <a id="migration-journals-and-tags"></a>Migration journals and tags | [Open section](docs/migration-state.md#migration-journals-and-tags) |
| <a id="migration-lint"></a>Migration lint | [Open section](docs/yql-reference.md#migration-lint) |
| <a id="migration-splits"></a>Migration splits | [Open section](docs/connections-and-execution.md#migration-splits) |
| <a id="native-cli-development-acceptance"></a>Native cli development acceptance | [Open section](CONTRIBUTING.md#host-driven-integration-checks) |
| <a id="native-coordination-planning-and-execution"></a>Native coordination planning and execution | [Open section](docs/native-objects.md#native-coordination-planning-and-execution) |
| <a id="native-protocol-generation"></a>Native protocol generation | [Open section](docs/coordination-models.md#native-protocol-generation) |
| <a id="native-row-table-creation"></a>Native row table creation | [Open section](docs/table-creation-and-rebuild.md#native-row-table-creation) |
| <a id="native-table-alteration"></a>Native table alteration | [Open section](docs/table-settings.md#native-table-alteration) |
| <a id="native-table-rebuild"></a>Native table rebuild | [Open section](docs/table-creation-and-rebuild.md#native-table-rebuild) |
| <a id="native-table-removal"></a>Native table removal | [Open section](docs/table-creation-and-rebuild.md#native-table-removal) |
| <a id="object-counts"></a>Object counts | [Open section](docs/catalog-and-errors.md#object-counts) |
| <a id="offline-capabilities"></a>Offline capabilities | [Open section](docs/yql-reference.md#offline-capabilities) |
| <a id="offline-column-declarations"></a>Offline column declarations | [Open section](docs/table-models.md#offline-column-declarations) |
| <a id="offline-column-storage-observations"></a>Offline column storage observations | [Open section](docs/table-models.md#offline-column-storage-observations) |
| <a id="offline-coordination-declarations"></a>Offline coordination declarations | [Open section](docs/coordination-models.md#offline-coordination-declarations) |
| <a id="offline-create-prediction"></a>Offline create prediction | [Open section](docs/table-creation-and-rebuild.md#offline-create-prediction) |
| <a id="offline-index-and-stream-declarations"></a>Offline index and stream declarations | [Open section](docs/table-models.md#offline-index-and-stream-declarations) |
| <a id="offline-native-object-declarations"></a>Offline native object declarations | [Open section](docs/table-models.md#offline-native-object-declarations) |
| <a id="offline-row-table-declarations"></a>Offline row table declarations | [Open section](docs/table-models.md#offline-row-table-declarations) |
| <a id="offline-schema-rendering"></a>Offline schema rendering | [Open section](docs/yql-reference.md#offline-schema-rendering) |
| <a id="offline-schema-validation"></a>Offline schema validation | [Open section](docs/table-creation-and-rebuild.md#offline-schema-validation) |
| <a id="offline-sql-analysis"></a>Offline sql analysis | [Open section](docs/yql-reference.md#offline-sql-analysis) |
| <a id="offline-table-preparation"></a>Offline table preparation | [Open section](docs/table-creation-and-rebuild.md#offline-table-preparation) |
| <a id="operational-storage-reservations"></a>Operational storage reservations | [Open section](docs/catalog-and-errors.md#operational-storage-reservations) |
| <a id="operator-statements-and-object-scopes"></a>Operator statements and object scopes | [Open section](docs/data-services.md#operator-statements-and-object-scopes) |
| <a id="partitioning-settings-read-replicas-and-key-bloom-filter"></a>Partitioning settings read replicas and key bloom filter | [Open section](docs/table-settings.md#partitioning-settings-read-replicas-and-key-bloom-filter) |
| <a id="resource-pools-and-classifiers"></a>Resource pools and classifiers | [Open section](docs/streaming-and-workload-objects.md#resource-pools-and-classifiers) |
| <a id="reverse-migrations"></a>Reverse migrations | [Open section](docs/migration-state.md#reverse-migrations) |
| <a id="row-table-model-conversion"></a>Row table model conversion | [Open section](docs/table-models.md#row-table-model-conversion) |
| <a id="rows-row-writes-and-seeds"></a>Rows row writes and seeds | [Open section](docs/data-services.md#rows-row-writes-and-seeds) |
| <a id="schema-observations"></a>Schema observations | [Open section](docs/catalog-and-errors.md#schema-observations) |
| <a id="secrets"></a>Secrets | [Open section](docs/native-objects.md#secrets) |
| <a id="serial-columns"></a>Serial columns | [Open section](docs/table-creation-and-rebuild.md#serial-columns) |
| <a id="sql-schema-reading"></a>Sql schema reading | [Open section](docs/yql-reference.md#sql-schema-reading) |
| <a id="statement-facts-for-ptah-sql-lint"></a>Statement facts for ptah sql lint | [Open section](docs/yql-reference.md#statement-facts-for-ptah-sql-lint) |
| <a id="streaming-queries"></a>Streaming queries | [Open section](docs/streaming-and-workload-objects.md#streaming-queries) |
| <a id="table-property-comparison"></a>Table property comparison | [Open section](docs/indexes-and-table-execution.md#table-property-comparison) |
| <a id="text-scanning"></a>Text scanning | [Open section](docs/yql-reference.md#text-scanning) |
| <a id="text-units"></a>Text units | [Open section](docs/connections-and-execution.md#text-units) |
| <a id="topics"></a>Topics | [Open section](docs/streaming-and-workload-objects.md#topics) |
| <a id="ttl"></a>Ttl | [Open section](docs/table-settings.md#ttl) |
| <a id="vector-and-full-text-indexes"></a>Vector and full text indexes | [Open section](docs/indexes-and-table-execution.md#vector-and-full-text-indexes) |

</details>
