ALTER TABLE `shop/users` DROP INDEX users_email;
ALTER TABLE `shop/users` DROP COLUMN email;
ALTER TABLE `shop/users` DROP COLUMN name;
ALTER TABLE `shop/users` RESET (TTL);
ALTER TABLE `shop/users` DROP COLUMN expires;
