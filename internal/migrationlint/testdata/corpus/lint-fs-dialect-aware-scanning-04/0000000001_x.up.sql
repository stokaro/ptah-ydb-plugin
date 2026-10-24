UPDATE cfg SET v = data #>> '{key}';
DROP TABLE legacy_audit;
