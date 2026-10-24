-- ptah:nolint DS102
ALTER TABLE users DROP COLUMN legacy;
-- atlas:nolint DS102
ALTER TABLE users DROP COLUMN legacy_two;
-- ptah:nolint DS101
DROP TABLE audit_log;
ALTER TABLE users ALTER COLUMN email TYPE TEXT;
