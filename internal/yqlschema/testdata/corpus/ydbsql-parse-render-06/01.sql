CREATE TABLE `events` (
    `id` Int64 NOT NULL,
    PRIMARY KEY (`id`)
);
ALTER TABLE `events` ADD CHANGEFEED `updates` WITH (MODE = 'NEW_AND_OLD_IMAGES', FORMAT = 'JSON');
ALTER TOPIC `events/updates` ADD CONSUMER `worker` WITH (read_from = Timestamp('2026-01-01T00:00:00Z'), availability_period = Interval('PT1H'));
