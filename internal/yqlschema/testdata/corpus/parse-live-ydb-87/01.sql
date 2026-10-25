CREATE VIEW `ptah_ydb_views/active` WITH (security_invoker = TRUE) AS
select   id,
  `label` -- the name shown
FROM `ptah_ydb_views/items`
WHERE qty>0