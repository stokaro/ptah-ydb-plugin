CREATE TABLE t (id Uint64 NOT NULL, emb String, PRIMARY KEY (id), INDEX t_emb GLOBAL USING vector_kmeans_tree ON (emb) WITH (distance = cosine, vector_type = "bit", vector_dimension = 8, levels = 1, clusters = 2));
CREATE TABLE u (id Uint64 NOT NULL, emb String, PRIMARY KEY (id));
ALTER TABLE u ADD INDEX u_emb GLOBAL USING Vector_KMeans_Tree ON (emb) WITH (similarity = inner_product, vector_type = 'float', vector_dimension = 3, levels = 1, clusters = 2);
UPSERT INTO u (id, emb) VALUES (1, 'x');
BATCH UPDATE u SET emb = 'y';
DELETE FROM u WHERE id = 1;
REPLACE INTO `u` (id) VALUES (2);
INSERT INTO $t (id) VALUES (3);
UPDATE t SET emb = 'z';
