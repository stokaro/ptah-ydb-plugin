CREATE TRANSFER ingest FROM tp TO t USING ($m) -> { return []; } WITH (CONNECTION_STRING = 'grpc://primary:2136/?database=/prod', TOKEN = 'secret');
