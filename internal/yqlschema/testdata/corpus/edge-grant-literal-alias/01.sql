CREATE TABLE t (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id));
GRANT 'select_row', 'ydb.generic.list' ON t TO alice;