CREATE TOPIC t1 (CONSUMER c1 WITH (type = 'shared'), CONSUMER c2 WITH (supported_codecs = 'raw,brotli'), CONSUMER c3) WITH (min_active_partitions = 1);
CREATE TOPIC IF NOT EXISTS t2 WITH (max_active_partitions = 5, auto_partitioning_strategy = 'scale_up');
CREATE TOPIC t3 WITH (max_active_partitions = 5, auto_partitioning_strategy = 'disabled');
CREATE TOPIC t4 WITH (supported_codecs = '  ');
CREATE TOPIC t5 WITH (supported_codecs = 'RAW, Gzip ,zstd', retention_period = Interval('PT1H'));
CREATE TOPIC t6 WITH (metering_mode = 'request_units');
