---
title: Embedding generations on YDB
description: The vector index a YDB embedding generation builds, and the refusals particular to YDB.
type: reference
audience:
  - application-developer
  - database-engineer
readerQuestion: "How does ptah inference index an embedding generation on YDB, and why does it refuse a specification?"
goal: "Write an embedding specification YDB accepts and fix one it refuses."
sourceOfTruth:
  - internal/embedstore
  - stokaro/ptah#4204
generated: false
overlaps: []
disposition: keep
sidebar:
  order: 4
---

# Embedding generations on YDB

`ptah inference` builds an embedding generation on YDB through this provider's
[embedding store](../README.md#embedding-store).
Ptah's [inference documentation](https://docs.ptah.run/edge/inference/overview/)
describes the lifecycle for every engine; this page gives the YDB index and the
refusals particular to YDB.

## The vector index

YDB has one vector index, `vector_kmeans_tree`, and a generation declares its
shape with two options, both required:

```yaml
target:
  representation: float
  metric: cosine
  index_method: vector_kmeans_tree
  index_options:
    levels: "2"
    clusters: "128"
```

`clusters` is how many clusters each level of the tree splits into, between 2
and 2048, and `levels` is how deep the tree goes, between 1 and 16.
`clusters` to the power of `levels` is at most 2^30. The metric becomes the
index's distance or similarity, and `vector_type` and `vector_dimension` come
from the representation and the provider's dimension. `ptah inference index`
builds it with `ALTER TABLE ... ADD INDEX ... GLOBAL USING vector_kmeans_tree`,
which returns once the index holds every row.

What the index can do depends on the YDB line, and Ptah reads it from the
server's capabilities:

| Line | Builds the index | Takes rows written after the build |
| --- | --- | --- |
| 25.1 | with `EnableVectorIndex` on | no |
| 25.2 | yes | no |
| 25.3 and later | yes | yes |

On a line whose index does not take later writes, a write into a generation
whose index exists is refused rather than left for the index to miss. The
lifecycle builds the index after the backfill, so the refusal appears only when
the source changed after the index was built.

## Troubleshooting

### `representation "vector" is not one a YDB generation stores`

**Cause.** The specification names a pgvector type and the database is YDB.

**Fix.** Set `target.representation: float`, `target.index_method:
vector_kmeans_tree` and the `levels` and `clusters` index options; see [The vector index](#the-vector-index).

### `builds generations in consistency mode immutable, and the specification selects "outbox"`

**Cause.** The specification selects `consistency.mode: outbox` and the
database is YDB, whose provider declares `immutable` alone. The outbox captures
a change with a row trigger in the transaction that makes it, and YDB has no
triggers.

**Fix.** Select `mode: immutable` and declare the source `mutable: false`, or
keep it mutable and stop its writes for the run with `consistency.paused: true`;
see
[Migrate a paused source](https://docs.ptah.run/edge/inference/guides/migrate-a-paused-source/).

### `key_fields [...] are not the primary key of ...`

**Cause.** On YDB the specification's `source.key_fields` name columns other
than the source table's primary key. A YDB generation reads by keyset over the
primary key and writes a row by it.

**Fix.** Name the primary key the message quotes, in key order.

### `has no vector_indexes, and building the generation's vector_kmeans_tree index needs it`

**Cause.** The YDB server cannot build a vector index. YDB 25.1 builds one only
with its `EnableVectorIndex` feature flag on, and Ptah reads the cluster's flags
only when the URL names its monitoring endpoint.

**Fix.** Turn the flag on, and add `monitoring=http://host:8765` to the
`--db-url` so the connection reads it.

### `has no vector_index_maintained_on_write`

**Cause.** A run wrote into a YDB generation whose index exists, on YDB 25.1 or
25.2. The index on those lines answers as it stood at its build, so the write
would leave it describing a table that has moved on. On a source declared
immutable this means the source changed after the index was built.

**Fix.** Build a new generation, in a column of its own, from the source as it
is now, or run the generation on YDB 25.3 or later, whose index takes later
writes in. Nothing was written: the refusal rolls the batch back.
