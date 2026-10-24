ALTER TABLE `shop/users` RENAME INDEX users_email TO users_email_v2;
ALTER TABLE `shop/users` DROP INDEX users_email_v2;
ALTER TABLE `shop/users` DROP COLUMN email;
