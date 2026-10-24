CREATE ASYNC REPLICATION mirror FOR a AS ra WITH (CONNECTION_STRING = 'grpc://primary:2136/?database=/prod');
