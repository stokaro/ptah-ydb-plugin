CREATE EXTERNAL DATA SOURCE `dir/bucket` WITH (
    SOURCE_TYPE = 'ObjectStorage',
    LOCATION = 'https://storage.invalid/root/',
    AUTH_METHOD = 'NONE'
);

CREATE EXTERNAL TABLE `dir/data.v1` (
    `d` Date,
    `body` Utf8
) WITH (
    DATA_SOURCE = 'dir/bucket',
    LOCATION = '/',
    CSV_DELIMITER = ' ',
    FORMAT = 'csv_with_names',
    PARTITIONED_BY = '["d"]'
);
