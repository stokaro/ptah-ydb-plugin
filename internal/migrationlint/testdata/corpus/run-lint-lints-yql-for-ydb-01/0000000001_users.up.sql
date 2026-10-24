CREATE TABLE `shop/users` (id Uint64 NOT NULL, email Utf8, PRIMARY KEY (id), INDEX users_email GLOBAL ON (email));
