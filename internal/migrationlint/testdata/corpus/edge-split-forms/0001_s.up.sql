--!syntax_v1
-- a header comment

-- attached to the first statement
CREATE TABLE `a\`b` (id Uint64 NOT NULL, PRIMARY KEY (id)); -- trails the first statement
/* a block comment
   on two lines */
UPSERT INTO `a\`b` (id) VALUES (1u);
$text = @@line one; still the literal
@@@@ and an escaped marker@@;
$f = ($x) -> { $y = $x + 1; RETURN $y; };
DEFINE ACTION $act($n) AS
  DO BEGIN
    SELECT CASE WHEN $n > 1 THEN 'a;b' ELSE "c;d" END AS case;
  END DO;
  SELECT 1 AS end;
END DEFINE;
EVALUATE FOR $i IN AsList(1, 2) DO $act($i);
EVALUATE IF true DO BEGIN SELECT 1; END DO;
;;
SELECT 'unterminated;
