CREATE TABLE `a.b` (
    `id` Int64 NOT NULL,
    `body` Utf8,
    PRIMARY KEY (`id`),
    INDEX `same` GLOBAL SYNC ON (`body`)
);
COMMENT ON TABLE `a.b` IS 'root; table';

CREATE TABLE `a/b` (
    `id` Int64 NOT NULL,
    `body` Utf8,
    PRIMARY KEY (`id`),
    INDEX `same` GLOBAL SYNC ON (`body`)
);
COMMENT ON COLUMN `a/b`.`body` IS 'line\nnext';
COMMENT ON INDEX `same` ON `a/b` IS 'nested index';

CREATE VIEW `a.v` WITH (security_invoker = TRUE) AS
SELECT id FROM `a.b`
;

CREATE VIEW `a/v` WITH (security_invoker = TRUE) AS
SELECT id FROM `a/b`
;
COMMENT ON VIEW `a/v` IS 'nested view';
