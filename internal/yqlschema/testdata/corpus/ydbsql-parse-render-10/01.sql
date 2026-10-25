CREATE TABLE `t` (
    `id` Uint64 NOT NULL,
    `ts` Timestamp,
    `body` Utf8 FAMILY `payload`,
    PRIMARY KEY (`id`),
    FAMILY `empty` (),
    FAMILY `payload` (COMPRESSION = 'lz4')
) WITH (TTL = Interval("PT1H") ON `ts`);
