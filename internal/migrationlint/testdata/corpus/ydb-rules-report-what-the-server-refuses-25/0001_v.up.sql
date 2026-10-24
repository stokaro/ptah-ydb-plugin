CREATE TABLE base (id Uint64 NOT NULL, PRIMARY KEY (id));
CREATE VIEW v WITH (security_invoker = TRUE) AS SELECT id FROM base;
