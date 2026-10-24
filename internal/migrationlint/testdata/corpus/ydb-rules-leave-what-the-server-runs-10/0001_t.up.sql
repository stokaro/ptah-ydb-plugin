CREATE TABLE `t` (
    `id` Uint64 NOT NULL,
    `a` Utf8 FAMILY `cold`,
    `c` Int32 FAMILY `cold` NOT NULL DEFAULT 7,
    PRIMARY KEY (`id`),
    FAMILY `cold` (DATA = 'hdd', COMPRESSION = 'lz4'),
    FAMILY `default` (COMPRESSION = 'lz4')
);
