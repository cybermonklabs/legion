-- 20261117111040 refuses a database it cannot read a pull request's keys in, with the line of
-- attribution_database_refusal(), which its DO block raises. This census answers 1 for each of
-- the two conditions of that refusal that the database meets, so the pre-deploy census stops the
-- deploy before the serving release stops: an encoding that is not UTF8, and a collation that
-- folds an ASCII letter in another way than "C" does (lower() or upper() of the 26 letters). It
-- counts no row: no stored row can fail another statement of the migration, whose header says
-- why for each.
--
-- The migration builds four indexes on tables that exist, pull_request_heads (number, and
-- lower(url) over URLs of at most 1,024 bytes) and delivery_pull_requests (number, and issue_key),
-- each reading its table under SHARE for its build, so the census holds both tables to its 1 GiB
-- limit. Its DO block is no catalog-only statement, so it also takes the table-size exemption and
-- the concurrent index exception from every later migration pending in the same deploy.
select (case when d.encoding <> pg_catalog.pg_char_to_encoding('UTF8') then 1 else 0 end)
     + (case when pg_catalog.lower('ABCDEFGHIJKLMNOPQRSTUVWXYZ') <> 'abcdefghijklmnopqrstuvwxyz'
              or pg_catalog.upper('abcdefghijklmnopqrstuvwxyz') <> 'ABCDEFGHIJKLMNOPQRSTUVWXYZ' then 1 else 0 end)
from pg_catalog.pg_database d
where d.datname = pg_catalog.current_database();
