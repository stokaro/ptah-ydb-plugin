ALTER TABLE `shop/users` ADD COLUMN score Int32 DEFAULT 0;
ALTER TABLE `shop/users` ADD INDEX users_score GLOBAL UNIQUE SYNC ON (score);
ALTER TABLE `shop/users` DROP COLUMN email;
