ALTER TABLE `shop/users` DROP INDEX users_name;
ALTER TABLE `shop/users` ADD INDEX users_name GLOBAL UNIQUE SYNC ON (name);
