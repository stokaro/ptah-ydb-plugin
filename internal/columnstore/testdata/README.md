# Column-table monitoring captures

`25-1.json`, `26-2-epoch.json`, `26-2-indexes.json`, `26-2-inline-indexes.json`
and `26-2-ttl.json` were copied unchanged from Ptah commit
`7b4b19f49fadf03ce81def2f6168ea60b3261a71`, `internal/ydbcolumn/testdata/`.
They are monitoring pages from YDB 25.1 and 26.2 with the path's own metadata
taken out, so the replay tests supply table-service descriptions of their own.

The other files are pairs read from one table each: `<name>.json` is the
`/viewer/json/describe?path=...&database=/local` page, unedited, raw access
lists included, and `<name>.table.json` is the table service's DescribeTable
result as protojson of `ColumnTableDescribeResult`. They were read from
`ydbplatform/local-ydb:25.1.4.7` and `ydbplatform/local-ydb:26.2.1.14`, started
with the feature flags of the provider's CI services; the 26.2 tables with a
tiered TTL were made after a dynamic configuration turned on
`enable_tiering_in_column_shard` and `enable_external_data_sources`. Each table
was made with YQL:

| Files | Table |
| --- | --- |
| `25-1-hash`, `26-2-hash` | `CREATE TABLE probe/c1 (id Uint64 NOT NULL, ts Timestamp NOT NULL, body Utf8, PRIMARY KEY (id, ts)) PARTITION BY HASH(id) WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3)` |
| `25-1-delete-ttl-inline` | `CREATE TABLE probe/c3 (ts Timestamp NOT NULL, id Uint64 NOT NULL, PRIMARY KEY (ts, id)) WITH (STORE = COLUMN, TTL = Interval("P1D") ON ts)` |
| `26-2-delete-ttl` | `probe/c3`, hashed by `id` in two shards, after a native AlterTable set a TTL of 7200 seconds on `ts` |
| `26-2-delete-ttl-inline` | `CREATE TABLE probe/c6 (...) PARTITION BY HASH(id) WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, TTL = Interval("PT3600S") ON ts)` |
| `26-2-tiered-inline` | `CREATE TABLE probe/c7 (...) PARTITION BY HASH(id) WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 1, TTL = Interval("P1D") TO EXTERNAL DATA SOURCE /local/probe/ext, Interval("P7D") DELETE ON ts)` |
| `26-2-tiered-altered` | `probe/c4`, keyed and hashed by `(ts, id)`, after `ALTER TABLE ... SET (TTL = Interval("PT86400S") TO EXTERNAL DATA SOURCE /local/probe/ext ON ts)` |
| `26-2-tiered-epoch` | `CREATE TABLE probe/c5 (ts Uint64 NOT NULL, id Uint64 NOT NULL, PRIMARY KEY (ts)) WITH (STORE = COLUMN, TTL = Interval("P1D") TO EXTERNAL DATA SOURCE /local/probe/ext ON ts AS SECONDS)` |
