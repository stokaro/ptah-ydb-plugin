CREATE TABLE `shop/users` (id Uint64 NOT NULL, email Utf8 NOT NULL, name Utf8, PRIMARY KEY (id), INDEX users_name GLOBAL ON (name));
