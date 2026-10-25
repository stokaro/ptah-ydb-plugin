CREATE TABLE events (id Int64 NOT NULL, PRIMARY KEY (id));ALTER TABLE events ADD CHANGEFEED updates WITH (
 mode = 'NEW_AND_OLD_IMAGES', format = 'JSON', virtual_timestamps = TRUE,
 resolved_timestamps = Interval('PT1S'), initial_scan = TRUE, user_sids = TRUE,
 schema_changes = TRUE, topic_min_active_partitions = 2, topic_auto_partitioning = 'ENABLED', retention_period = Interval('PT12H'));
 ALTER TOPIC `events/updates` ADD CONSUMER audit WITH (important = TRUE, supported_codecs = 'raw,gzip');