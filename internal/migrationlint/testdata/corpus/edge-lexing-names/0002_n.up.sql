CREATE TABLE `q\`t` (id Uint64 NOT NULL, `a\\b` Utf8, PRIMARY KEY (id), INDEX `i\`x` GLOBAL ON (`a\\b`));
COMMENT ON COLUMN `q\`t`.`a\\b` IS 'x';
COMMENT ON INDEX `i\`x` ON `q\`t` IS 'y';
