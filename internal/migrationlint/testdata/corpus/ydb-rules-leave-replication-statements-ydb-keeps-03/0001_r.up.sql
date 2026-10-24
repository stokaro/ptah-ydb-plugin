CREATE ASYNC REPLICATION mirror FOR a AS ra WITH (CONNECTION_STRING = 'grpc://primary:2136/?database=/prod', USER = 'u', PASSWORD_SECRET_NAME = 'pw');
CREATE TRANSFER ingest FROM tp TO t USING ($m) -> { return []; } WITH (CONNECTION_STRING = 'grpc://primary:2136/?database=/prod', TOKEN_SECRET_PATH = 'secrets/token');
