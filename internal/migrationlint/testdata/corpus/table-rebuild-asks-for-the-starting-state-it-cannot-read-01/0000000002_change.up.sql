CREATE TABLE `app/__ptah_rebuild_items` (
    `id` Int64 NOT NULL,
    `n` Int64,
    `label` Utf8,
    PRIMARY KEY (`id`)
);
INSERT INTO `app/__ptah_rebuild_items` (`id`, `n`, `label`) SELECT Unwrap(`id`, 'rebuilding table app.items: column id holds NULL'u) AS `id`, IF(`n` IS NULL, NULL, Unwrap(CAST(`n` AS Int64), 'rebuilding table app.items: column n does not convert'u)) AS `n`, `label` AS `label` FROM `app/items`;
ALTER TABLE `app/items` RENAME TO `app/__ptah_replaced_items`;
ALTER TABLE `app/__ptah_rebuild_items` RENAME TO `app/items`;
DROP TABLE `app/__ptah_replaced_items`;
