-- atlas:nolint destructive

DROP TABLE users;
ALTER TABLE accounts DROP COLUMN legacy;
