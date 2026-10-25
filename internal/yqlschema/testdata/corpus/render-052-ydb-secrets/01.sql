-- Statement 1/2
CREATE SECRET `pg_password` WITH (value = $PTAH_SECRET_PG_PASSWORD);


-- Statement 2/2
CREATE SECRET `ext/s3_key` WITH (value = $PTAH_SECRET_S3_KEY);


