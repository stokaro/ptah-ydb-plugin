CREATE VIEW v WITH (security_invoker = TRUE) AS DO BEGIN
  $x = 1;
  SELECT $x AS x;
END DO;
CREATE VIEW w WITH (security_invoker = TRUE) AS SELECT ($f) -> { RETURN $f; } AS g;