CREATE TABLE `t` (
    `id` Uint64 NOT NULL,
    `ts` Uint64,
    PRIMARY KEY (`id`)
) WITH (TTL = Interval("PT1H") ON `ts` AS SECONDS);
