CREATE TABLE `dir/t` (
    `id` Int64 NOT NULL,
    `ts` Timestamp,
    PRIMARY KEY (`id`)
) WITH (TTL = Interval("P1D") ON `ts`);
