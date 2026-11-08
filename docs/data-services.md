---
title: YDB data and object scope services
description: Row typing and writes, embedding generations, and object selection rules.
type: reference
audience:
  - database-engineer
  - schema-author
readerQuestion: "How does the provider handle rows, embeddings, and object scope?"
goal: "Find YDB data and selection service rules."
sourceOfTruth:
  - internal/backend
  - internal/inspect
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 17
---

# YDB data and object scope services

This reference records the provider's YDB-specific behavior. For a first connection, start with [Using the provider with Ptah](usage.md). The [documentation index](README.md) links the task-focused guides.

References to the former embedded YDB engine describe the historical [Ptah implementation at 7b4b19f](https://github.com/stokaro/ptah/tree/7b4b19f49fadf03ce81def2f6168ea60b3261a71); current Ptah uses this provider.

## Rows, row writes and seeds

The provider serves the row services of the contract (`data.type-rows`,
`data.read-rows`, `data.render-writes`) and the seed service (`data.seeds`),
with the answers of the host's embedded YDB engine: its row typing
(`internal/ydbdata`), its row diff statements (`migration/datadiff`) and its
seeds (`migration/seeder` and the YDB seed service). The host keeps which rows
are declared, the row diff, the ordering and the seed policy.

The engine declares the host's YDB query dialect for the host's query
builder, and a session reports the capabilities the builder reads, at the
host's line presets: `returning_clause` from 25.3, since 25.1 and 25.2 fail
`UPDATE ... RETURNING` on a table with a unique index (`wrong returning expr
type`), and `common_table_expressions`, `correlated_subqueries`,
`non_equi_joins` and `offset_without_limit` on no line. The builder refuses a
clause whose key a session leaves out.

YQL types every literal and converts few of them, so every value is compared
and written in its column's YDB type: the live table's type where the column
exists, and otherwise the type the declared type lands on for the request's
capabilities (`VARCHAR(255)` is Utf8, `TIMESTAMP` is Timestamp64 with
`wide_date_time_types` and Timestamp without it, `DECIMAL(10,2)` needs
`parameterized_decimal`). `TypeRows` returns the declared and the live rows in
one canonical form, so a declared `12.50` pairs with the `12.5` a Decimal reads
back, a moment in another zone with the one in UTC a read returns, `PT26H`
with the interval of the same length and an upper-case UUID with the lower-case
one YDB stores.

A Decimal is a decimal at its column's scale, a Date a date, the
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
complete page that says so. A Decimal value with more digits than its
precision, which is how YDB stores infinity and NaN, fails the read rather than
being returned as digits the column never held.

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

## Operator statements and object scopes

The `sessions.statements` service runs operator checks, migration test
assertions and script steps on a pinned connection. It returns typed rows in
bounded pages and leaves the affected-row count absent because YDB does not
report it. A result ends at its last page, explicit close, transaction end or
session close. Cleanup failures remain failures on a repeated close.

A read-only request proves that its single SELECT reads local tables or views,
then runs in a snapshot read-only transaction. External tables, views that
reach them, UDFs, code evaluation and file or secret functions are refused
before execution. A transport failure has an unknown outcome and is never
retried. Confirmed atomic query failures preserve the server's issue text for
expected-error assertions, without the driver's address and transport wrapper.
Bound arguments retain JSON, decimal, date, timestamp and interval types;
precision the engine cannot represent is refused before execution.

The provider offers `sessions.typed-arguments`. A host that lists it sends each
argument with the type its caller bound it at, and the provider binds the
parameter at that YDB type: `INT8` to `INT64` as `Int8` to `Int64`, `UINT8` to
`UINT64` as `Uint8` to `Uint64`, `FLOAT32` as `Float`, `FLOAT64` as `Double`,
`STRING` as `Utf8`, `BYTES` as `String`, `TIMESTAMP` as `Timestamp` and
`INTERVAL` as `Interval`. An optional type binds as `Optional<T>` and its null
as a null of `T`, a list as `List<T>` (an empty one keeps its element type),
and an untyped null as `Void`, as a plain null binds. So a Go `int32` writes an
`Int32` column, which an `Int64` parameter cannot, and a nil `*string` writes a
null into an `Optional<Utf8>` one. A typed timestamp outside the `Timestamp`
range is refused rather than bound as a `Timestamp64`.

The negotiated `scope.selections` declaration makes secrets, streaming queries,
coordination nodes, topics, external data sources, external tables, async
replications and transfers selectable by their directory and name. Resource
pools and classifiers use database-wide names, including literal dots. A kept
classifier or streaming query keeps the pool it requires when the input
contains that pool, the pool `default` for a query that names none; an
explicit exclusion of the pool still wins. A table's changefeeds go with the
table. An external table keeps no data
source and a data source no secret: an `--include` of a table plans that
table alone, whether it is created or dropped, as the host's embedded engine
did.

The host keeps a requirement on each side of a comparison alone, so a
kept source would be dropped with a table the declaration leaves out, and
created again beside a held one. Grants follow the object their
target names: each kind except the classifier declares the grant object type
the host's embedded engine names it with, such as `ASYNC REPLICATION` or
`TRANSFER`. A grant on a type no selection declares is read as a grant on a
table of the same name, so a scoped plan would leave it out.
