CREATE STREAMING QUERY `jobs/copy.v1` WITH (RUN=FALSE, RESOURCE_POOL=default) AS DO BEGIN
$f = ($x) -> { RETURN $x + 1; }; /* keep ; */ INSERT INTO sink SELECT $f(id) FROM source;
END DO; CREATE TOPIC source;