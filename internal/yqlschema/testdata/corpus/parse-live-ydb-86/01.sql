CREATE VIEW `ptah_ydb_views/active_count` WITH (security_invoker = TRUE) AS
SELECT COUNT(*) AS n FROM `ptah_ydb_views/active`