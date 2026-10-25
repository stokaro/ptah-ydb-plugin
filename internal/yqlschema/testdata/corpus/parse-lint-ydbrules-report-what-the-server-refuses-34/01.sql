CREATE TABLE `dir/t` (
    `id` Int64 NOT NULL,
    `e` Uint64,
    PRIMARY KEY (`id`)
) WITH (TTL = Interval("PT1H") ON `e` AS SECONDS)