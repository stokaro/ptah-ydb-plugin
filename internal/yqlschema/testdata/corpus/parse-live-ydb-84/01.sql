CREATE VIEW `ptah_ydb_comments/active_users` WITH (security_invoker = TRUE) AS
SELECT id FROM `ptah_ydb_comments/users`