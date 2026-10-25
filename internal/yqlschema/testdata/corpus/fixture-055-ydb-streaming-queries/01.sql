CREATE STREAMING QUERY `stream_copy` WITH (RUN = FALSE) AS DO BEGIN
    INSERT INTO stream_output SELECT * FROM stream_input;
END DO;
