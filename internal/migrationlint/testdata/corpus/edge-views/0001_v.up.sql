CREATE TABLE t (id Uint64 NOT NULL, PRIMARY KEY (id));
CREATE TABLE u (id Uint64 NOT NULL, PRIMARY KEY (id));
CREATE VIEW v1 WITH (security_invoker = TRUE) AS SELECT t.id FROM t JOIN u ON t.id = u.id;
CREATE VIEW v2 WITH (security_invoker = TRUE) AS SELECT * FROM u;
CREATE VIEW IF NOT EXISTS v2 WITH (security_invoker = TRUE) AS SELECT * FROM t;
CREATE VIEW v3 WITH (security_invoker = TRUE) AS SELECT * FROM $src;
CREATE VIEW v4 WITH (security_invoker = TRUE) AS SELECT * FROM AS_TABLE($rows);
