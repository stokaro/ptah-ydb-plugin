-- detached by a blank line

DROP TOPIC t; DROP TOPIC u; -- after the last statement on its line
-- right above the next one
DROP TRANSFER x
;
DROP COORDINATION NODE IF EXISTS `dir/node`;
DROP STREAMING QUERY q;
DROP ASYNC REPLICATION r CASCADE;
DROP ASYNC REPLICATION r2;
/* unterminated block comment ; DROP TOPIC hidden;