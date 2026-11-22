ALTER TABLE users ALTER COLUMN email TYPE TEXT; -- ptah:nolint destructive/drop-column
ALTER TABLE users DROP COLUMN legacy;
