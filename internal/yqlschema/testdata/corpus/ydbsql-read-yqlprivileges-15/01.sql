CREATE TOPIC source; CREATE STREAMING QUERY `jobs/copy` WITH (RUN=FALSE) AS DO BEGIN
INSERT INTO sink SELECT * FROM source;
END DO; REVOKE 'manage' ON `jobs/copy` FROM operators;