CREATE TABLE t (id Uint64 NOT NULL, a Utf8, b Utf8, c Utf8, d Utf8, PRIMARY KEY (id), INDEX t_a GLOBAL ON (a), INDEX t_b GLOBAL ON (b));
COMMENT ON COLUMN `t`.`a` IS 'the a column';
COMMENT ON COLUMN `t`.`b` IS "escaped \x41 \n \t \\ \' \"";
COMMENT ON INDEX `t_a` ON `t` IS 'an index comment';
COMMENT ON INDEX `t_b` ON `t` IS 'another';
COMMENT ON TABLE `t` IS 'the table';
comment on column `t`.`c` is 'lower case keywords';
COMMENT ON COLUMN `/local/t`.`d` IS 'absolute path';
COMMENT ON VIEW `t` IS 'a view comment on a table name';
