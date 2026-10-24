CREATE TABLE `app/notes` (id Uint64 NOT NULL, body Utf8, tag Utf8, PRIMARY KEY (id), INDEX notes_tag GLOBAL ON (tag));
COMMENT ON COLUMN `app/notes`.`body` IS 'The text';
COMMENT ON INDEX `notes_tag` ON `app/notes` IS 'By tag';
ALTER TABLE `app/notes` DROP COLUMN body;
