-- Statement 1/1
CREATE TABLE `memberships` (
    `org_id` Int32 NOT NULL,
    `user_id` Int32 NOT NULL,
    `role` Utf8 NOT NULL,
    PRIMARY KEY (`org_id`, `user_id`)
);


