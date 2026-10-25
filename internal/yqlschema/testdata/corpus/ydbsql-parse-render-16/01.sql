CREATE TABLE `t` (
    `ts` Timestamp NOT NULL,
    `id` Uint64,
    PRIMARY KEY (`ts`)
) PARTITION BY HASH (`ts`) WITH (TTL = Interval("PT1H") ON `ts`, STORE = COLUMN);
