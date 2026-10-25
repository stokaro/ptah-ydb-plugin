CREATE COORDINATION NODE `app/locks` WITH (self_check_period = Interval('PT0.5S'), session_grace_period = Interval('PT2S'));
CREATE COORDINATION NODE `deep/er/node`;
CREATE COORDINATION NODE `a.b`;