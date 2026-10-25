CREATE TABLE t (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id));
GRANT SELECT ON `/other/t` TO alice;