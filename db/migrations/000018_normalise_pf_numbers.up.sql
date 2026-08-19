-- ---------------------------------------------------------------------------
-- 000018 Store PF account numbers in their readable form
--
-- The EPFO portal exports a member account number run together —
-- MHBAN00123450000012345 — while people write and read it with separators:
-- MH/BAN/0012345/000/0012345. Both refer to the same account.
--
-- Storing one shape means a search for either finds the record, and a list does
-- not mix the two. The API now normalises on write (httpx.NormalisePFNumber);
-- this brings existing rows into line.
--
-- Only rows already in the compact 22-character shape are touched. Anything
-- else — already separated, or an entry that never matched the format — is left
-- exactly as typed, because rewriting an unrecognised value would lose data
-- rather than tidy it.
-- ---------------------------------------------------------------------------

UPDATE users
SET pf_number = CONCAT_WS('/',
        SUBSTRING(pf_number, 1, 2),    -- region,        e.g. MH
        SUBSTRING(pf_number, 3, 3),    -- office,        e.g. BAN
        SUBSTRING(pf_number, 6, 7),    -- establishment
        SUBSTRING(pf_number, 13, 3),   -- extension
        SUBSTRING(pf_number, 16, 7))   -- member id
WHERE pf_number IS NOT NULL
  AND pf_number REGEXP '^[A-Za-z]{5}[0-9]{17}$';
