-- ptah:nolint destructive/drop-column
ALTER TABLE users DROP COLUMN legacy;
-- atlas:nolint destructive/drop-column
ALTER TABLE users DROP COLUMN legacy_two;
-- ptah:nolint destructive/drop-table
DROP TABLE audit_log;
ALTER TABLE users ALTER COLUMN email TYPE TEXT;
