CREATE TABLE t (id Uint64 NOT NULL, a Utf8, PRIMARY KEY (id), INDEX t_a GLOBAL ON (a));
COMMENT ON INDEX `t_a` ON `t` IS 'kept on the index';
COMMENT ON COLUMN `t`.`a` IS 'kept on the column';
