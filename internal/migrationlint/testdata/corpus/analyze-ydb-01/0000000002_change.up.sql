ALTER TABLE `shop/users` ADD INDEX users_email GLOBAL UNIQUE SYNC ON (email);
DROP TABLE `shop/legacy`;
