-- ---------------------------------------------------------------------------
-- Reverse of 000018: strip the separators back out.
-- ---------------------------------------------------------------------------

UPDATE users
SET pf_number = REPLACE(pf_number, '/', '')
WHERE pf_number IS NOT NULL
  AND pf_number REGEXP '^[A-Za-z]{2}/[A-Za-z]{3}/[0-9]{7}/[0-9]{3}/[0-9]{7}$';
