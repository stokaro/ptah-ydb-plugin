CREATE TABLE `shop/orders` (id BigSerial NOT NULL, PRIMARY KEY (id));
CREATE TABLE counters (id serial4 NOT NULL, n SmallSerial, PRIMARY KEY (id));
CREATE TABLE tags (id Serial2 NOT NULL, PRIMARY KEY (id));
ALTER SEQUENCE `/local/counters/_serial_column_n` INCREMENT BY 2;
ALTER SEQUENCE `/local/shop/orders/_serial_column_id` RESTART WITH 100;
ALTER SEQUENCE `/local/shop/orders/_serial_column_id` INCREMENT BY 3;
