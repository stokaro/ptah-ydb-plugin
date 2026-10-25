-- Statement 1/2
CREATE TABLE `accounts` (
    `id` Serial NOT NULL,
    `name` Utf8 NOT NULL,
    PRIMARY KEY (`id`)
);


-- Statement 2/2
CREATE TABLE `users` (
    `id` Serial NOT NULL,
    `account_id` Int32,
    `manager_id` Int32,
    `email` Utf8 NOT NULL,
    PRIMARY KEY (`id`)
);


