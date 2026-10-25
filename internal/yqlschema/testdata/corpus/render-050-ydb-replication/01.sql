-- Statement 1/4
CREATE TABLE `order_log` (
    `partition` Uint32 NOT NULL,
    `offset` Uint64 NOT NULL,
    `message` Utf8,
    PRIMARY KEY (`partition`, `offset`)
);


-- Statement 2/4
CREATE TABLE `orders` (
    `id` Int64 NOT NULL,
    `total` Int64,
    PRIMARY KEY (`id`)
);
ALTER TABLE `orders` ADD CHANGEFEED `feed` WITH (MODE = 'NEW_IMAGE', FORMAT = 'JSON');


-- Statement 3/4
CREATE ASYNC REPLICATION `mirror` FOR `accounts` AS `replica/accounts`, `/prod/ledger` AS `replica/ledger` WITH (CONNECTION_STRING = 'grpcs://primary.example.com:2135/?database=/prod', USER = 'replicator', PASSWORD_SECRET_PATH = 'secrets/replicator', CONSISTENCY_LEVEL = 'GLOBAL', COMMIT_INTERVAL = Interval('PT30S'));


-- Statement 4/4
CREATE TRANSFER `order_transfer` FROM `orders/feed` TO `order_log` USING ($msg) -> { return [<| partition: $msg._partition, offset: $msg._offset, message: CAST($msg._data AS Utf8) |>]; } WITH (BATCH_SIZE_BYTES = 1048576, FLUSH_INTERVAL = Interval('PT10S'));


