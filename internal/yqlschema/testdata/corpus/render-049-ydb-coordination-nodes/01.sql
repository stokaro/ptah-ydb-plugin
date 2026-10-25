-- Statement 1/3
CREATE TABLE `app/jobs` (
    `id` Int64 NOT NULL,
    `state` Utf8,
    PRIMARY KEY (`id`)
);


-- Statement 2/3
CREATE COORDINATION NODE `app_locks`;


-- Statement 3/3
CREATE COORDINATION NODE `app/limits` WITH (self_check_period = Interval('PT2S'), session_grace_period = Interval('PT15S'), read_consistency_mode = 'strict', attach_consistency_mode = 'relaxed', rate_limiter_counters_mode = 'detailed');


