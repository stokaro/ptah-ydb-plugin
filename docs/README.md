# YDB provider documentation

These pages and the repository [README](../README.md) document what YDB does in
Ptah: which YQL a plan renders, which changes the provider plans in place and
which it refuses, the capabilities each release line and each feature flag
gives, and what is not supported yet. Ptah's own documentation, at
[docs.ptah.run](https://docs.ptah.run/edge/), describes the commands, the source
formats and the pipeline every engine shares; its
[Configure the YDB provider](https://docs.ptah.run/edge/extend/providers/ydb/)
page installs and selects this provider.

| Topic | Where |
| --- | --- |
| Using the provider from `ptah` and `ptah-compat` | [Using the provider with Ptah](ptah.md) |
| Go annotations and YAML keys that declare YDB objects | [Declaring YDB objects](declarations.md) |
| How a migration is split, the migration lock, statement and lock timeouts | [Migrations on YDB](migrations.md) |
| The vector index of an embedding generation | [Embedding generations on YDB](inference.md) |
| Releases, platforms and the minimum contract | [Releases](../README.md#releases) |
| Connection addresses and feature flags | [Connection addresses](../README.md#connection-addresses), [Feature flags](../README.md#feature-flags) |
| Dev realms, dev databases and their cleanup | [Dev realms](../README.md#dev-realms), [Dev databases](../README.md#dev-databases) |
| Execution, transactions and migration bodies | [Execution and transactions](../README.md#execution-and-transactions), [Migration splits](../README.md#migration-splits) |
| Migration history and the migration lock | [Migration history](../README.md#migration-history), [Coordination locks](../README.md#coordination-locks) |
| Views, comments, users, groups and permissions | [Common objects](../README.md#common-objects) |
| Row tables, keys, types and Serial columns | [Native row-table creation](../README.md#native-row-table-creation), [Serial columns](../README.md#serial-columns) |
| Column families, TTL, partitioning, read replicas and key bloom filter | [Native table alteration](../README.md#native-table-alteration) |
| A table's TTL in HCL, its `platform "ydb"` block | [Offline row-table declarations](../README.md#offline-row-table-declarations) |
| Indexes, index partitioning, vector and full-text indexes | [Index alteration](../README.md#index-alteration), [Vector and full-text indexes](../README.md#vector-and-full-text-indexes) |
| Table rebuilds | [Native table rebuild](../README.md#native-table-rebuild) |
| Column tables | [Column tables](../README.md#column-tables) |
| Changefeeds | [Changefeeds](../README.md#changefeeds) |
| Coordination nodes | [Offline coordination declarations](../README.md#offline-coordination-declarations), [Native coordination planning and execution](../README.md#native-coordination-planning-and-execution) |
| Resource pools, classifiers and streaming queries | [Resource pools and classifiers](../README.md#resource-pools-and-classifiers), [Streaming queries](../README.md#streaming-queries) |
| Async replications and transfers | [Async replications and transfers](../README.md#async-replications-and-transfers) |
| Secrets | [Secrets](../README.md#secrets) |
| External data sources and external tables | [External data sources and external tables](../README.md#external-data-sources-and-external-tables) |
| Desired YQL schema files | [SQL schema reading](../README.md#sql-schema-reading) |
| Migration lint and `ptah sql lint` | [Migration lint](../README.md#migration-lint), [Statement facts for `ptah sql lint`](../README.md#statement-facts-for-ptah-sql-lint) |
| Seeds, declared rows and the query builder | [Rows, row writes and seeds](../README.md#rows-row-writes-and-seeds) |
| Embedding generations | [Embedding store](../README.md#embedding-store) |
| What the provider does not cover yet | [Extraction still required](../README.md#extraction-still-required) |

The YDB release lines and their capabilities are this provider's to declare;
its release publishes them in `release-lines.json`, and Ptah's support matrix
lists only the engines Ptah builds in.
