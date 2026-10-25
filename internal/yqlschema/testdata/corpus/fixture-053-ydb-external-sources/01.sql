CREATE EXTERNAL DATA SOURCE `ext/warehouse` WITH (
    SOURCE_TYPE = 'PostgreSQL',
    LOCATION = 'pg.example.test:5432',
    AUTH_METHOD = 'BASIC',
    DATABASE_NAME = 'app',
    LOGIN = 'reader',
    PASSWORD_SECRET_PATH = 'ext/pg_password'
);

CREATE EXTERNAL DATA SOURCE `ext/events_bucket` WITH (
    SOURCE_TYPE = 'ObjectStorage',
    LOCATION = 'https://storage.example.test/events/',
    AUTH_METHOD = 'NONE'
);

CREATE EXTERNAL TABLE `ext/events` (
    `id` Int64 NOT NULL,
    `kind` Utf8,
    `amount` Decimal(22,9)
) WITH (
    DATA_SOURCE = 'ext/events_bucket',
    LOCATION = '2026/',
    FORMAT = 'json_each_row',
    COMPRESSION = 'gzip'
);
