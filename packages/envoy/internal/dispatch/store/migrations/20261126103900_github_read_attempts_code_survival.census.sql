-- 20261126103900 replaces github_read_attempts_read_check with one that admits a fifth read,
-- `code_survival`, and writes no row. The check being replaced admits four of the five names the
-- new one admits, so no stored row is refused, and the migration rewrites none. The census cannot
-- name github_read_attempts, which 20261118020802 creates and which does not exist when a
-- deployment that has not applied that migration takes this census.
select 0;
