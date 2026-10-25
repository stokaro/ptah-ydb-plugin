CREATE TABLE t (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id));
GRANT SELECT ON `/local/t` TO alice;