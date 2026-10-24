CREATE TABLE __ptah_rebuild_base (id Uint64 NOT NULL, n Int64, PRIMARY KEY (id));
INSERT INTO __ptah_rebuild_base (id, n) SELECT id, CAST(n AS Int64) AS n FROM base;
ALTER TABLE base RENAME TO __ptah_replaced_base;
ALTER TABLE __ptah_rebuild_base RENAME TO base;
DROP TABLE __ptah_replaced_base;
