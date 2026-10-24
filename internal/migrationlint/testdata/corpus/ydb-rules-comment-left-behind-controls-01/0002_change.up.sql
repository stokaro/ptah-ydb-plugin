ALTER TABLE `app/notes` RENAME INDEX `notes_tag` TO `notes_by_tag`;
ALTER TABLE `app/notes` DROP COLUMN `body`;
COMMENT ON COLUMN `app/notes`.`body` IS NULL;
COMMENT ON INDEX `notes_by_tag` ON `app/notes` IS 'By tag';
COMMENT ON INDEX `notes_tag` ON `app/notes` IS NULL;
