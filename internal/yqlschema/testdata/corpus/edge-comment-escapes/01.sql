CREATE TABLE t (id Uint64 NOT NULL, v Utf8, PRIMARY KEY (id));
COMMENT ON COLUMN `t`.`v` IS 'line\nnext \x41';