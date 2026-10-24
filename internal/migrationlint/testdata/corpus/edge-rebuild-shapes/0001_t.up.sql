CREATE TABLE t (id Uint64 NOT NULL, a Utf8, b Utf8, PRIMARY KEY (id));
CREATE TABLE r (id Uint64 NOT NULL, PRIMARY KEY (id));
CREATE VIEW vt WITH (security_invoker = TRUE) AS SELECT * FROM t;
