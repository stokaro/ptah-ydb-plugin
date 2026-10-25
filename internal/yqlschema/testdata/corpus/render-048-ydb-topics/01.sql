-- Statement 1/1
CREATE TOPIC `app/order_events` (CONSUMER `billing` WITH (important = TRUE), CONSUMER `audit` WITH (read_from = Timestamp('2026-01-01T00:00:00Z'))) WITH (min_active_partitions = 2, retention_period = Interval('P1DT12H'), supported_codecs = 'raw,gzip');


