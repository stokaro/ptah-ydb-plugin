CREATE TABLE `t` (
    `id` Utf8 NOT NULL,
    PRIMARY KEY (`id`)
) WITH (PARTITION_AT_KEYS = (('a b'u)));
