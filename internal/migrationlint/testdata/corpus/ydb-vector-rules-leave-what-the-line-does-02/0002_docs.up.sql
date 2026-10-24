ALTER TABLE docs DROP INDEX docs_emb;
UPSERT INTO docs (id, emb) VALUES (1, "x");
