CREATE TABLE `ptah_ydb_comments/users` (
    `id` Int64 NOT NULL,
    `email` Utf8,
    `label` Utf8,
    `legacy` Utf8,
    PRIMARY KEY (`id`),
    INDEX `users_by_label` GLOBAL SYNC ON (`label`),
    INDEX `users_by_legacy` GLOBAL SYNC ON (`legacy`),
    INDEX `users_email_key` GLOBAL UNIQUE SYNC ON (`email`)
)