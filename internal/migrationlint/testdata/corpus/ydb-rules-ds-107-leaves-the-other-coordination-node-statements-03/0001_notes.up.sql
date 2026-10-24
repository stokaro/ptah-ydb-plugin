CREATE TABLE notes (id Uint64 NOT NULL, body Utf8, PRIMARY KEY (id));
UPSERT INTO notes (id, body) VALUES (1, 'DROP COORDINATION NODE locks'u);
