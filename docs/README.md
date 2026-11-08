# YDB provider documentation

The repository [README](../README.md) is the short installation and first-use
guide. These pages describe YDB-specific declarations, operations, limits,
and diagnostics. Ptah's own documentation, at
[docs.ptah.run](https://docs.ptah.run/edge/), describes the commands, the source
formats and the pipeline every engine shares; its
[Configure the YDB provider](https://docs.ptah.run/edge/extend/providers/ydb/)
page installs and selects this provider.

| Topic | Where |
| --- | --- |
| Using the provider from `ptah` and `ptah-compat` | [Using the provider with Ptah](usage.md) |
| What topics, consumers, changefeeds, transfers and async replications are, for a relational reader | [YDB objects for relational readers](objects.md) |
| Go annotations and YAML keys that declare YDB objects | [Declaring YDB objects](declarations.md) |
| How a migration is split, the migration lock, statement and lock timeouts | [Migrations on YDB](migrations.md) |
| The vector index of an embedding generation | [Embedding generations on YDB](inference.md) |
| Connections, execution, migration state, and catalog errors | [Connections and execution](connections-and-execution.md), [Migration state](migration-state.md), [Catalog and errors](catalog-and-errors.md) |
| Tables and coordination models | [Table models](table-models.md), [Coordination models](coordination-models.md) |
| Native and streaming objects | [Native objects](native-objects.md), [Streaming and workload objects](streaming-and-workload-objects.md) |
| Table changes and indexes | [Creation and rebuild](table-creation-and-rebuild.md), [Table settings](table-settings.md), [Indexes and execution](indexes-and-table-execution.md) |
| YQL and data services | [YQL reference](yql-reference.md), [Data services](data-services.md) |
| Releases, platforms and the minimum contract | [Releases](../CONTRIBUTING.md#releases) |
| Connection addresses and feature flags | [Connection addresses](connections-and-execution.md#connection-addresses), [Feature flags](connections-and-execution.md#feature-flags) |
| Dev realms, dev databases and their cleanup | [Dev realms](connections-and-execution.md#dev-realms), [Dev databases](connections-and-execution.md#dev-databases) |
| Execution, transactions and migration bodies | [Execution and transactions](connections-and-execution.md#execution-and-transactions), [Migration splits](connections-and-execution.md#migration-splits) |
| Migration history and the migration lock | [Migration history](migration-state.md#migration-history), [Coordination locks](migration-state.md#coordination-locks) |
| Views, comments, users, groups and permissions | [Common objects](migration-state.md#common-objects) |
| Row tables, keys, types and Serial columns | [Native row-table creation](table-creation-and-rebuild.md#native-row-table-creation), [Serial columns](table-creation-and-rebuild.md#serial-columns) |
| Column families, TTL, partitioning, read replicas and key bloom filter | [Native table alteration](table-settings.md#native-table-alteration) |
| A table's TTL in HCL, its `platform "ydb"` block | [Offline row-table declarations](table-models.md#offline-row-table-declarations) |
| Indexes, index partitioning, vector and full-text indexes | [Index alteration](indexes-and-table-execution.md#index-alteration), [Vector and full-text indexes](indexes-and-table-execution.md#vector-and-full-text-indexes) |
| Table rebuilds | [Native table rebuild](table-creation-and-rebuild.md#native-table-rebuild) |
| Column tables | [Column tables](table-models.md#column-tables) |
| Changefeeds | [Changefeeds](native-objects.md#changefeeds) |
| Coordination nodes | [Offline coordination declarations](coordination-models.md#offline-coordination-declarations), [Native coordination planning and execution](native-objects.md#native-coordination-planning-and-execution) |
| Resource pools, classifiers and streaming queries | [Resource pools and classifiers](streaming-and-workload-objects.md#resource-pools-and-classifiers), [Streaming queries](streaming-and-workload-objects.md#streaming-queries) |
| Async replications and transfers | [Async replications and transfers](streaming-and-workload-objects.md#async-replications-and-transfers) |
| Secrets | [Secrets](native-objects.md#secrets) |
| External data sources and external tables | [External data sources and external tables](native-objects.md#external-data-sources-and-external-tables) |
| Desired YQL schema files | [SQL schema reading](yql-reference.md#sql-schema-reading) |
| Migration lint and `ptah sql lint` | [Migration lint](yql-reference.md#migration-lint), [Statement facts for `ptah sql lint`](yql-reference.md#statement-facts-for-ptah-sql-lint) |
| Seeds, declared rows and the query builder | [Rows, row writes and seeds](data-services.md#rows-row-writes-and-seeds) |
| Embedding generations | [Embedding store](data-services.md#embedding-store) |
| Current status and important limits | [Current status](../README.md#current-status) |

The YDB release lines and their capabilities are this provider's to declare;
its release publishes them in `release-lines.json`, and Ptah's support matrix
lists only the engines Ptah builds in.

## How these pages reach docs.ptah.run

Every page in this folder except this README is published on docs.ptah.run
under `extend/providers/ydb/<page>/`, beside Ptah's own pages, and
[Configure the YDB provider](https://docs.ptah.run/edge/extend/providers/ydb/)
introduces them. The pages live here so that a change to the provider and a
change to its documentation are one pull request.

Ptah's documentation build imports this folder at the release of this provider
that `docs/site/scripts/data/provider-docs.json` in stokaro/ptah names. A merged
change here therefore reaches the site when Ptah declares a release that
carries it: the pull request that moves the declaration to the new release
imports its pages, records any new page's address, and adds a redirect for a
page this folder removed or renamed. Each released version of Ptah's
documentation keeps the pages of the release it declared.

A page here is written for both places:

- It starts with the frontmatter every page of the site carries: `title`,
  `description`, `type`, `audience`, `readerQuestion`, `goal`,
  `sourceOfTruth`, `generated: false`, `overlaps` and `disposition`.
  `sidebar.order` places it in the provider's part of the navigation.
- Its `# Title` heading repeats `title`, so the page reads on GitHub; the site
  renders the title from the frontmatter and drops the heading.
- It links another page of this folder by its file name, any other file of this
  repository by its relative path, and a Ptah page by its address on
  `https://docs.ptah.run/edge/`. The import turns each into an address that
  works on the site: a route of the site, or the file on GitHub at the imported
  commit. `sourceOfTruth` names paths relative to the repository root.

Ptah's
[documentation site README](https://github.com/stokaro/ptah/blob/master/docs/site/README.md#provider-documentation)
lists what the import accepts and refuses.

`.github/workflows/docs.yml` runs Ptah's documentation checks on this folder in
every pull request, from Ptah's own `provider-docs.yml` workflow: the style,
terminology and link checks, the command lines a page publishes, and a build of
the whole site with these pages in it. The checkers stay in Ptah. To read a page
on the site before a release declares it, import this checkout into a Ptah
checkout:

```console
cd docs/site
npm ci
node scripts/import-provider-docs.mjs --source ydb=/path/to/ptah-ydb-plugin
npm run dev
```
