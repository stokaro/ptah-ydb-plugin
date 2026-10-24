ALTER TABLE `dir/t` ADD COLUMN `ts` Timestamp;
ALTER TABLE `dir/t` SET (TTL = Interval("P1D") ON `ts`);
ALTER TABLE `dir/t` DROP COLUMN `ts`;
