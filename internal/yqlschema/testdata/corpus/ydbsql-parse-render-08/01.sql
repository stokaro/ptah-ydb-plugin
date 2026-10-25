CREATE TABLE `t` (
    `id` Int64 NOT NULL,
    `value` Utf8 DEFAULT 'active'u,
    PRIMARY KEY (`id`),
    INDEX `i` GLOBAL SYNC ON (`value`)
);
