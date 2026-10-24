CREATE FUNCTION noop() RETURNS void AS $ptah$
BEGIN
    -- DROP TABLE decoy; inside a dollar-quoted body and a comment
    PERFORM 1;
END;
$ptah$ LANGUAGE plpgsql;
