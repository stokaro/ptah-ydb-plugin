# Go export of the host's embedded YDB engine

`tables.txt` holds the table declarations that `ptah introspect` of Ptah at
host commit 74de0b65b (the last commit with the embedded YDB engine) wrote for
one directory, `ex`. The output was the same on local-ydb 25.1.4.7 and
26.2.1.14, run with `enable_resource_pools` beside the CI feature flags,
because that engine refuses a Go export while resource pools are unavailable.
`TestHostGoExportWritesWhatTheEmbeddedEngineWrites_HappyPath` compares the
provider's export with these lines.

To record it again, build `./cmd/ptah` at 74de0b65b, create the tables below in
the directory `ex`, and keep the part of the generated file from the first
`//ptah:schema:table` line on:

```sql
CREATE TABLE `ex/plain` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id));
CREATE TABLE `ex/tuned` (id Uint64 NOT NULL, body Utf8, created_at Timestamp, PRIMARY KEY (id),
    INDEX by_body GLOBAL ON (body), FAMILY cold (COMPRESSION = "lz4"))
  WITH (TTL = Interval("P30D") ON created_at, AUTO_PARTITIONING_BY_LOAD = ENABLED,
    AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2, READ_REPLICAS_SETTINGS = "ANY_AZ:1", KEY_BLOOM_FILTER = ENABLED);
ALTER TABLE `ex/tuned` ALTER INDEX by_body SET (AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 3);
CREATE TABLE `ex/cols` (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id))
  PARTITION BY HASH (id) WITH (STORE = COLUMN, AUTO_PARTITIONING_MIN_PARTITIONS_COUNT = 2);
CREATE TABLE `ex/colttl` (ts Timestamp NOT NULL, id Uint64 NOT NULL, PRIMARY KEY (ts, id))
  PARTITION BY HASH (id) WITH (STORE = COLUMN, TTL = Interval("P1D") ON ts);
CREATE TABLE `ex/kinds` (id Uint64 NOT NULL, a Utf8, b Utf8, PRIMARY KEY (id),
    INDEX by_a GLOBAL ASYNC ON (a), INDEX by_b GLOBAL UNIQUE SYNC ON (b));
CREATE TABLE `ex/rowkey` (a Uint64 NOT NULL, b Uint64 NOT NULL, v Utf8, PRIMARY KEY (a, b));
CREATE TABLE `ex/rowkeyrev` (a Uint64 NOT NULL, b Uint64 NOT NULL, PRIMARY KEY (b, a));
```

```sh
ptah introspect --db-url 'ydb://127.0.0.1:2136/local?monitoring=http://127.0.0.1:8765' --schemas ex --out out --single-file
```
