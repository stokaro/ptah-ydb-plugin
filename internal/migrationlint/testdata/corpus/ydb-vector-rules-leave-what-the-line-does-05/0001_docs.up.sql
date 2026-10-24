CREATE TABLE docs (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id), INDEX docs_v GLOBAL ON (v));
UPSERT INTO docs (id, v) VALUES (1, "x"u);
