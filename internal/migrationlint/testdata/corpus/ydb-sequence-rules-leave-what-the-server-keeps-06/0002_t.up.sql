DROP TABLE `shop/orders`;
CREATE TABLE `shop/orders` (id BigSerial NOT NULL, PRIMARY KEY (id));
ALTER SEQUENCE `/local/shop/orders/_serial_column_id` INCREMENT BY 10;
