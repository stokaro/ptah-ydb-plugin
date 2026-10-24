-- atlas:txtar

-- migration.sql --
DROP TABLE users;

-- down.sql --
CREATE TABLE users (id INTEGER PRIMARY KEY);
