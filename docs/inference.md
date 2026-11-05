# Embedding generations on YDB

`ptah inference` builds an embedding generation on YDB through this provider's
[embedding store](https://github.com/stokaro/ptah-ydb-plugin#embedding-store).
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
