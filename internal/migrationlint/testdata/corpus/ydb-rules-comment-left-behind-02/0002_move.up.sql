ALTER TABLE `app/notes` RENAME TO `app/old_notes`;
ALTER TABLE `app/old_notes` DROP COLUMN body;
