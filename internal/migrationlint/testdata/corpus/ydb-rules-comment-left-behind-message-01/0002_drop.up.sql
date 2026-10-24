ALTER TABLE `app/notes` DROP COLUMN body;
ALTER TABLE `app/notes` RENAME INDEX notes_tag TO by_tag;
