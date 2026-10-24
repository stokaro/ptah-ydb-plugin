CREATE TABLE `shop/users` (
  id Uint64 NOT NULL, email Utf8, name Utf8, score Int32, expires Timestamp,
  PRIMARY KEY (id),
  INDEX users_email GLOBAL SYNC ON (email) COVER (name)
) WITH (TTL = Interval("P30D") ON expires);
CREATE VIEW `shop/active` WITH (security_invoker = TRUE) AS SELECT id FROM `shop/users`;
CREATE VIEW `shop/named` WITH (security_invoker = TRUE) AS SELECT id, name FROM `shop/users`;
