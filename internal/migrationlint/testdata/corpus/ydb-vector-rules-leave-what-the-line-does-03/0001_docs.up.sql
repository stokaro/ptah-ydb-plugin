CREATE TABLE docs (id Uint64 NOT NULL, emb String, PRIMARY KEY (id),
  INDEX docs_emb GLOBAL USING vector_kmeans_tree ON (emb)
  WITH (distance=cosine, vector_type=float, vector_dimension=3, levels=1, clusters=2));
UPSERT INTO docs (id, emb) VALUES (1, "x");
ALTER TABLE docs ADD INDEX docs_bits GLOBAL USING vector_kmeans_tree ON (emb) WITH (distance=manhattan, vector_type=bit, vector_dimension=8, levels=1, clusters=2);
