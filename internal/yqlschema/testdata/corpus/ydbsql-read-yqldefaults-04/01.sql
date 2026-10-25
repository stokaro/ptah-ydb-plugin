CREATE TABLE t (id Int64 NOT NULL, v String DEFAULT @@first
DELIMITER $$
last@@, PRIMARY KEY (id));