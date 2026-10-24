CREATE COORDINATION NODE `app/locks`;
ALTER COORDINATION NODE `app/locks` SET (session_grace_period = Interval('PT20S'));
