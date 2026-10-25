CREATE TABLE `app/t` (id Uint64 NOT NULL, PRIMARY KEY (id));
CREATE VIEW `app/v` WITH (security_invoker = TRUE) AS SELECT 1 AS a;
CREATE COORDINATION NODE `app/locks`;
CREATE USER alice;
GRANT SELECT, 'ydb.granular.update_row' ON `app/t`, `app/v` TO alice;
GRANT ALL PRIVILEGES ON `app` TO alice WITH GRANT OPTION;
GRANT CONNECT ON `/Root/db` TO alice;
GRANT DESCRIBE SCHEMA ON `app/locks` TO alice;