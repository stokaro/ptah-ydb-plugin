---
title: Declaring YDB objects
description: The Go annotation directives and YAML keys that declare an object or a setting only YDB has.
type: reference
audience:
  - schema-author
  - database-engineer
readerQuestion: "Which directives and YAML keys declare YDB objects such as column families, changefeeds and topics?"
goal: "Declare a YDB-only object in a Go or YAML schema source."
sourceOfTruth:
  - internal/declaration
  - internal/ydbdecl
  - internal/sourceencode
  - schema
  - stokaro/ptah#4204
generated: false
searchAliases:
  - "YDB column family"
  - "YDB changefeed"
  - "YDB topic"
overlaps: []
disposition: keep
sidebar:
  order: 3
---

# Declaring YDB objects

Ptah's source formats carry the YDB objects and settings this provider claims.
Ptah reads them only when a command selects the provider: a `ydb://` or
`ydbs://` URL, `--dialect ydb`, or a `docker://ydb/...` dev URL.

This page lists the Go annotation directives and the YAML keys that declare an
object only YDB has. [YDB objects for relational readers](objects.md) explains
what each object is and what it needs. The attributes YDB adds to a directive or key every engine
has, such as `on_topic` on a grant, `group` on a role, or the `COVER` columns
of an index, stay with that directive in Ptah's
[Go annotation reference](https://docs.ptah.run/edge/reference/go-annotations/) and
[YAML schema reference](https://docs.ptah.run/edge/reference/yaml-schema/), and
so do the rules every directive follows: placement, `dialects`, and the
`platform.ydb` group of a table.

A declaration the provider refuses prints a reason after the error code: the
object by its key or name, the attribute, and what the attribute takes, in the
words of Ptah's built-in YDB engine. The reason never quotes the value the
declaration wrote:

```text
provider decode source failed (InvalidArgument): topic "events": invalid retention_period: takes an ISO 8601 duration such as PT12H or P1D
```

A Go directive that leaves out an attribute the tables below mark as required
is refused with `missing required annotation attribute "rank"`, and Ptah names
the directive and its place after the reason.

## Go annotations

### `//ptah:schema:columnfamily`

Declares a YDB column family: columns a row table stores together, with a
storage pool, a compression and a cache mode of their own. It belongs to the
table of the struct it is on, or to the one `table` names, which the same file
declares. Every other dialect refuses a table that declares one. See
[column families](../README.md#column-families).

| Attribute | Required | Description |
| --- | --- | --- |
| `name` | Yes | Family name. `default` sets the family that holds the key and every unlisted column. |
| `table` | No | Table the family belongs to, when not the struct's own. |
| `data` | No | Kind of storage pool the family is kept in, such as `ssd`. Omitted, the table keeps the pool it holds. |
| `compression` | No | `off` or `lz4`. Omitted, the table keeps the compression it holds. |
| `cache_mode` | No | `regular` or `in_memory`, either of which needs `column_family_cache_mode`. Omitted, the table keeps the cache mode it holds. |
| `fields` | No | Columns the family holds, never a key column, and none another family lists. The default family lists none. |

### `//ptah:schema:changefeed`

Declares a YDB changefeed: a stream of a table's changes, which YDB keeps in a
topic at `<table>/<name>`. It belongs to the table of the struct it is on, or
to the one `table` names, which the same file declares. Every other dialect
refuses a table that declares one. See
[changefeeds](../README.md#changefeeds).

| Attribute | Required | Description |
| --- | --- | --- |
| `name` | Yes | Changefeed name, unique among the table's changefeeds and indexes. |
| `mode` | Yes | What a record carries: `KEYS_ONLY`, `UPDATES`, `NEW_IMAGE`, `OLD_IMAGE` or `NEW_AND_OLD_IMAGES`. |
| `format` | Yes | How a record is written: `JSON` or `DEBEZIUM_JSON`. |
| `table` | No | Table the changefeed belongs to, when not the struct's own. |
| `virtual_timestamps` | No | Each record carries its change's virtual timestamp. `true`/`false`; bare form allowed. |
| `resolved_timestamps` | No | Interval of the barrier records, an ISO 8601 duration of whole seconds. |
| `retention_period` | No | How long the topic keeps a record; 24 hours when omitted. |
| `initial_scan` | No | The stream opens with a record for every row. `true`/`false`; bare form allowed. |
| `user_sids` | No | Each record names its change's user. Needs `changefeed_user_sids`. |
| `schema_changes` | No | The stream carries schema change records. Needs `changefeed_schema_changes`. |
| `topic_min_active_partitions` | No | Partitions the topic starts with, from 1 to 9223372036854775807. |
| `topic_auto_partitioning` | No | The topic gains partitions as writes grow. Needs `changefeed_topic_auto_partitioning`. |

### `//ptah:schema:changefeed:consumer`

Declares a consumer of a changefeed's topic: a named reader that keeps its own
position in the stream.

| Attribute | Required | Description |
| --- | --- | --- |
| `name` | Yes | Consumer name, unique within the topic. |
| `changefeed` | Yes | Changefeed whose topic the consumer reads. |
| `table` | No | Table of the changefeed, when not the struct's own. |
| `important` | No | The topic keeps what this consumer has not read past the retention. `true`/`false`; bare form allowed. |
| `read_from` | No | RFC 3339 time a partition this consumer has not read is read from, in whole seconds. |
| `supported_codecs` | No | Codecs the consumer reads: `raw`, `gzip`, `lzop`, `zstd`, `custom`. |
| `availability_period` | No | How long the topic keeps what this consumer has not read past the retention. Needs `topic_consumer_availability_period`. |

### `//ptah:schema:topic`

Declares a YDB topic: a persistent message queue at a path of the database. A
setting left out stands for the value YDB gives a new topic. Every target but
YDB refuses a topic. See [Topics](../README.md#topics) for what a plan
does with one.

| Attribute | Required | Description |
| --- | --- | --- |
| `auto_partitioning_down_utilization_percent` | No | Share of a partition's write speed below which partitions merge, 1 to 100. |
| `auto_partitioning_stabilization_window` | No | How long a load lasts before the partition count follows it, an ISO 8601 duration. |
| `auto_partitioning_strategy` | No | `disabled`, `scale_up`, `scale_up_and_down` or `paused`. |
| `auto_partitioning_up_utilization_percent` | No | Share of a partition's write speed above which it splits, 1 to 100. |
| `max_active_partitions` | No | Most partitions auto-partitioning splits the topic into; needs a strategy other than `disabled`. |
| `min_active_partitions` | No | Partitions writers write to. The count only grows. |
| `name` | Yes | Topic name, the last segment of its path. |
| `partition_write_burst_bytes` | No | Burst a partition takes above its quota, in bytes; the write speed when omitted. |
| `partition_write_speed_bytes_per_second` | No | Write quota of one partition, in bytes per second. |
| `retention_period` | No | How long the topic keeps a message, an ISO 8601 duration; 24 hours when omitted. |
| `schema` | No | Directory that holds the topic, relative to the database root. |
| `supported_codecs` | No | Codecs a writer may use: `raw`, `gzip`, `lzop`, `zstd`, `custom`. |

### `//ptah:schema:topic:consumer`

Declares a consumer of a topic declared in the same file: a named reader with a
read position of its own.

| Attribute | Required | Description |
| --- | --- | --- |
| `availability_period` | No | How long the topic keeps a message this consumer has not read past the retention period, an ISO 8601 duration. YDB 25.4 and later. |
| `important` | No | The topic keeps a message this consumer has not read past the retention period. `true`/`false`. |
| `name` | Yes | Consumer name, unique within the topic. |
| `read_from` | No | RFC 3339 time a partition this consumer has not read is read from. |
| `schema` | No | Directory of the topic, when it has one. |
| `supported_codecs` | No | Codecs the consumer reads: `raw`, `gzip`, `lzop`, `zstd`, `custom`. |
| `topic` | Yes | Topic the consumer reads. |

### `//ptah:schema:resourcepool`

Declares a YDB resource pool, which limits the queries that run in it. A pool
belongs to the whole database, and a setting left out has no limit. YDB keeps
pools behind the `EnableResourcePools` flag; see
[resource pools](../README.md#resource-pools-and-classifiers).

| Attribute | Required | Description |
| --- | --- | --- |
| `name` | Yes | Pool name; `default` changes the pool YDB creates. |
| `concurrent_query_limit` | No | Most queries that run at once, a whole number. |
| `queue_size` | No | Most queries that wait; needs `concurrent_query_limit` or `database_load_cpu_threshold`. |
| `database_load_cpu_threshold` | No | Database CPU load in percent above which new queries wait. |
| `query_memory_limit_percent_per_node` | No | Share of a node's memory one query may take, in percent. |
| `query_cpu_limit_percent_per_node` | No | Share of a node's CPU one query may take, in percent. |
| `total_cpu_limit_percent_per_node` | No | Share of a node's CPU the pool's queries take together. |
| `resource_weight` | No | The pool's share of the CPU when pools compete, in percent. |

### `//ptah:schema:resourcepool:classifier`

Declares a YDB resource pool classifier, which sends a user's or a group's
queries to a pool. Of the classifiers that match a query, the lowest rank
decides.

| Attribute | Required | Description |
| --- | --- | --- |
| `name` | Yes | Classifier name. |
| `resource_pool` | Yes | Pool the queries go to: a declared pool or `default`. |
| `member_name` | No | User or group whose queries it matches; every query when omitted. |
| `rank` | Yes | Order among the classifiers, from 0; unique. |

### `//ptah:schema:coordinationnode`

Declares a YDB coordination node, which holds an application's semaphores and
rate limiter resources. A setting left out takes YDB's default.

| Attribute | Required | Description |
| --- | --- | --- |
| `attach_consistency_mode` | No | `strict` or `relaxed`. YDB's default is `strict`. |
| `name` | Yes | Node name. |
| `rate_limiter_counters_mode` | No | `aggregated` or `detailed`. YDB's default is `aggregated`. |
| `read_consistency_mode` | No | `strict` or `relaxed`. YDB's default is `relaxed`. |
| `schema` | No | Directory holding the node, relative to the database root. |
| `self_check_period` | No | How often the node checks it is alive, as an ISO 8601 duration from `PT0.5S` to `PT10S`. YDB's default is `PT1S`. |
| `session_grace_period` | No | How long a session keeps its semaphores while the node changes its leader, from the self-check period plus one second to `PT30S`. YDB's default is `PT10S`. |

```go
//ptah:schema:coordinationnode name="locks" schema="app" self_check_period="PT2S"
type Locks struct{}
```

A coordination node is YDB's own object, and every other target refuses the
declaration. `ptah_locks` at the database root is Ptah's lock node and is
refused. [Coordination nodes](../README.md#native-coordination-planning-and-execution) says how
Ptah applies one.
### `//ptah:schema:async_replication`

Declares a YDB async replication: tables of another database copied into
read-only replica tables YDB creates and keeps current. The tables it copies
are declared with `//ptah:schema:async_replication:item` in the same file, and
the replica tables are not declared as tables. A credential names a secret and
never holds a value. Every target but YDB refuses a replication. See
[Async replications and transfers](../README.md#async-replications-and-transfers)
for what a plan does with one.

| Attribute | Required | Description |
| --- | --- | --- |
| `commit_interval` | No | How often a global replication commits, an ISO 8601 duration; ten seconds when omitted. |
| `connection_string` | Yes | The other database: `grpc://host:port/?database=/path` or `grpcs://...`. |
| `consistency_level` | No | Consistency of the replica: `row` or `global`; `row` when omitted. |
| `name` | Yes | Replication name, the last segment of its path. |
| `password_secret_name` | No | Object secret holding the user's password. |
| `password_secret_path` | No | Path of a secret holding the user's password, relative to the database root (YDB 25.4 and later). |
| `schema` | No | Directory that holds it, relative to the database root. |
| `token_secret_name` | No | Object secret holding an access token. |
| `token_secret_path` | No | Path of a secret holding an access token, relative to the database root (YDB 25.4 and later). |
| `user` | No | User a password secret signs in as. |

### `//ptah:schema:async_replication:item`

Declares one table, or directory of tables, an async replication copies, and
where its replica is created. The replication is declared in the same file.

| Attribute | Required | Description |
| --- | --- | --- |
| `replication` | Yes | Replication the item belongs to. |
| `schema` | No | Directory of the replication, when it has one. |
| `source` | Yes | Path in the source database, relative to its root or absolute. |
| `target` | Yes | Path of the replica in this database, relative to its root. |

### `//ptah:schema:transfer`

Declares a YDB transfer: messages of a topic turned into rows of a table
through a YQL lambda. The table is declared in the same schema, and so is the
topic, as a topic or as a changefeed, unless the transfer reads another
database or a topic or changefeed the database holds and Ptah cannot describe,
which the plan keeps. Every target but YDB refuses a transfer.
[What a comparison may conclude](https://docs.ptah.run/edge/extend/write-a-provider/ydb-incomplete-knowledge/)
shows each refusal with the test that measures it.

| Attribute | Required | Description |
| --- | --- | --- |
| `batch_size_bytes` | No | Bytes the transfer gathers before a write; 8 MiB when omitted. |
| `connection_string` | No | The other database, for a topic outside this one: `grpc://host:port/?database=/path` or `grpcs://...`. |
| `consumer` | No | Existing topic consumer the transfer reads through; YDB creates one when omitted. |
| `flush_interval` | No | Longest wait before a write, an ISO 8601 duration of whole seconds; a minute when omitted. |
| `name` | Yes | Transfer name, the last segment of its path. |
| `password_secret_name` | No | Object secret holding the user's password. |
| `password_secret_path` | No | Path of a secret holding the user's password, relative to the database root (YDB 25.4 and later). |
| `schema` | No | Directory that holds it, relative to the database root. |
| `source` | Yes | Topic the transfer reads, relative to the database root; for a topic of another database, relative to its root or absolute. |
| `target` | Yes | Table the transfer writes, relative to the database root. |
| `token_secret_name` | No | Object secret holding an access token. |
| `token_secret_path` | No | Path of a secret holding an access token, relative to the database root (YDB 25.4 and later). |
| `user` | No | User a password secret signs in as. |
| `using` | Yes | The YQL lambda, written inline: `($msg) -> { ... }`. |

### `//ptah:schema:secret`

Declares a YDB secret: a scheme object whose value the server keeps and never
returns, which an external data source reads a password or a key from. The
declaration names the environment variable that holds the value, and never the
value itself. Every other dialect refuses a secret. See
[secrets](../README.md#secrets).

| Attribute | Required | Description |
| --- | --- | --- |
| `name` | Yes | Secret name, the last segment of its path. A dot is part of the name; a slash is refused. |
| `schema` | No | Directory that holds the secret, relative to the database root. |
| `value_env` | Yes | Environment variable that holds the value. Its name starts with `PTAH_SECRET_`. |

A `value` attribute is refused, and the error names the attribute and not what
it held. The secret's path is `schema/name`, which is how every other place
names it: a limit, `--rotate-secret` and every message.

### `//ptah:schema:externaldatasource`

Declares a YDB external data source: another system YDB reads from, such as an
object storage bucket or a PostgreSQL database, and how YDB authenticates to
it. A credential is named by the secret that holds it, never written. Every
other dialect refuses one. See
[external data sources](../README.md#external-data-sources-and-external-tables).

| Attribute | Required | Description |
| --- | --- | --- |
| `name` | Yes | Data source name, the last segment of its path. |
| `schema` | No | Directory that holds the data source, relative to the database root. |
| `source_type` | Yes | `SOURCE_TYPE`, such as `ObjectStorage`, `PostgreSQL` or `ClickHouse`. |
| `location` | No | `LOCATION`: the bucket's address or the server's host and port. |
| `auth_method` | Yes | `AUTH_METHOD`, such as `NONE`, `BASIC` or `SERVICE_ACCOUNT`. |
| `options` | No | Every other option, `NAME=value` separated by `;`. `\;` writes a semicolon into a value. |

A data source declared twice, by any two declarations of any source format, is
refused. A PostgreSQL source whose password the secret `ext/pg_password`
holds:

```go
//ptah:schema:externaldatasource name="warehouse" schema="ext" source_type="PostgreSQL" location="pg:5432" auth_method="BASIC" options="DATABASE_NAME=app;LOGIN=reader;PASSWORD_SECRET_PATH=ext/pg_password"
type Warehouse struct{}
```

### `//ptah:schema:externaltable`

Declares a YDB external table: columns over files that an external data source
of type `ObjectStorage` holds. YDB stores no row of it. Every other dialect
refuses one.

| Attribute | Required | Description |
| --- | --- | --- |
| `name` | Yes | External table name, the last segment of its path. |
| `schema` | No | Directory that holds the external table, relative to the database root. |
| `data_source` | Yes | Path of the data source the table reads, relative to the database root. |
| `location` | Yes | `LOCATION`: the files' path under the data source. |
| `columns` | Yes | Columns, `name Type [NOT NULL]` separated by commas, such as `id Int64 NOT NULL, amount Decimal(22,9)`. A default, a key or a column family is refused. |
| `options` | No | Every other option, such as `FORMAT` and `COMPRESSION`, `NAME=value` separated by `;`. |

## YAML keys

### Column families

A YDB table's column families sit under `tables.<table>.column_families`, keyed
by name. The keys are the attributes of `//ptah:schema:columnfamily`, with the
same values; `fields` is a list of the columns the family holds. Every other
dialect refuses a table that declares a column family. See
[column families](../README.md#column-families).

```yaml
tables:
  documents:
    column_families:
      default:
        compression: lz4
      cold:
        data: hdd
        compression: lz4
        fields: [body, attachment]
```

### Changefeeds

A YDB table's changefeeds sit under `tables.<table>.changefeeds`, keyed by
name, and each changefeed's consumers under its `consumers` map, keyed by
name. The keys are the attributes of `//ptah:schema:changefeed` and
`//ptah:schema:changefeed:consumer`, with the same values; `supported_codecs`
is a list. Every other dialect refuses a table that declares a changefeed. See
[changefeeds](../README.md#changefeeds).

```yaml
tables:
  orders:
    changefeeds:
      updates:
        mode: NEW_AND_OLD_IMAGES
        format: JSON
        retention_period: PT12H
        consumers:
          billing:
            important: true
          search:
            supported_codecs: [raw, gzip]
```

### Secrets

A YDB secret sits under `secrets`, keyed by name, with the attributes of
`//ptah:schema:secret`: `name` when the key is not the name, `schema` for its
directory, and `value_env` for the environment variable that holds the value,
whose name starts with `PTAH_SECRET_`. A document never holds the value: a
`value` key is refused, and the error names the key and not what it held. A
dot is part of the name, and a slash in it is refused. Every other dialect
refuses a secret. See [secrets](../README.md#secrets).

```yaml
secrets:
  pg_password:
    schema: ext
    value_env: PTAH_SECRET_PG_PASSWORD
```

### External data sources and tables

A YDB external data source sits under `external_data_sources` and an external
table under `external_tables`, each keyed by name, with the attributes of
`//ptah:schema:externaldatasource` and `//ptah:schema:externaltable`. `options`
is a map of option names, in any letter case, to values, and each column of a
table has a `name`, a `type` and `not_null`. Every other dialect refuses both.
See [external data sources](../README.md#external-data-sources-and-external-tables).

```yaml
external_data_sources:
  events_bucket:
    schema: ext
    source_type: ObjectStorage
    location: https://storage.example.test/events/
    auth_method: NONE
external_tables:
  events:
    schema: ext
    data_source: ext/events_bucket
    location: 2026/
    columns:
      - {name: id, type: Int64, not_null: true}
      - {name: kind, type: Utf8}
    options:
      FORMAT: json_each_row
```

### Column storage

A table's `schema` names its database schema or YDB directory. Its
`column_store` block selects column storage. `hash_columns` names the
hash key, `partitions` sets its initial shard count, and `ttl` holds an optional
eviction policy. Each TTL tier declares an `interval` and an absolute
`external_source` path; omitting the source means deletion and is allowed only
on the last tier. A deletion-only policy is the table's TTL in its `ydb`
platform group instead.

Local indexes use `type: bloom_filter`, `bloom_ngram_filter`, or `min_max`.
Bloom methods accept `false_positive_probability`; the n-gram method also
accepts `ngram_size` and `case_sensitive`. See the
[YDB provider's documentation](../README.md#column-tables)
for limits, cluster flags and examples.
